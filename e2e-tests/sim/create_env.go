package sim

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"

	"vim-arcade.theprimeagen.com/pkg/assert"
	"vim-arcade.theprimeagen.com/pkg/dummy"
	gameserverstats "vim-arcade.theprimeagen.com/pkg/game-server-stats"
	"vim-arcade.theprimeagen.com/pkg/matchmaking"
	servermanagement "vim-arcade.theprimeagen.com/pkg/server-management"
)

type ServerState struct {
    Sqlite *gameserverstats.Sqlite
    Server *servermanagement.LocalServers
    MatchMaking *matchmaking.MatchMakingServer
    Port int
    Factory *TestingClientFactory
    Conns ConnMap
}

func (s *ServerState) Close() {
    s.MatchMaking.Close()
    s.Server.Close()

    err := s.Sqlite.Close()
    assert.NoError(err, "sqlite errored on close")
}

func (s *ServerState) String() string {
    configs, err := s.Sqlite.GetAllGameServerConfigs()
    configsStr := strings.Builder{}
    if err != nil {
        _, err = configsStr.WriteString(fmt.Sprintf("unable to get server configs: %s", err))
        assert.NoError(err, "never should happen (famous last words)")
    } else {
        for i, c := range configs {
            if i > 0 {
                configsStr.WriteString("\n")
            }
            configsStr.WriteString(c.String())
        }
    }

    connections := s.Sqlite.GetTotalConnectionCount()
    return fmt.Sprintf(`ServerState:
Connections: %s
Servers
%s
`, connections.String(), configsStr.String())
}

type TestingClientFactory struct {
    host string
    port uint16
    logger *slog.Logger
}

func NewTestingClientFactory(host string, port uint16, logger *slog.Logger) TestingClientFactory {
    return TestingClientFactory{
        logger: logger.With("area", "TestClientFactory"),
        host: host,
        port: port,
    }
}

func (f *TestingClientFactory) CreateBatchedConnections(count int) []*dummy.DummyClient {
    conns := make([]*dummy.DummyClient, 0)

    wait := sync.WaitGroup{}
    wait.Add(count)
    f.logger.Info("creating all clients", "count", count)
    for range count {
        conns = append(conns, f.NewWait(&wait))
    }
    wait.Wait()
    f.logger.Info("clients all created", "count", count)

    return conns
}


func (f TestingClientFactory) WithPort(port uint16) TestingClientFactory {
    f.port = port
    return f
}

func (f *TestingClientFactory) New() *dummy.DummyClient {
    client := dummy.NewDummyClient(f.host, f.port)
    f.logger.Info("factory connecting", "id", client.ConnId)
    client.Connect(context.Background())
    f.logger.Info("factory connected", "id", client.ConnId)
    return &client
}

// this is getting hacky...
func (f *TestingClientFactory) NewWait(wait *sync.WaitGroup) *dummy.DummyClient {
    client := dummy.NewDummyClient(f.host, f.port)
    f.logger.Info("factory new client with wait", "id", client.ConnId)

    go func() {
        defer wait.Done()

        f.logger.Info("factory client connecting with wait", "id", client.ConnId)
        client.Connect(context.Background())
        f.logger.Info("factory client connected with wait", "id", client.ConnId)
    }()

    return &client
}

func createServer(ctx context.Context, server *ServerState, logger *slog.Logger) (string, *gameserverstats.GameServerConfig) {
    logger.Info("creating server")
    sId, err := server.Server.CreateNewServer(ctx)
    logger.Info("created server", "id", sId, "err", err)
    assert.NoError(err, "unable to create server")
    logger.Info("waiting server...", "id", sId)
    server.Server.WaitForReady(ctx, sId)
    logger.Info("server ready", "id", sId)
    sConfig := server.Sqlite.GetById(sId)
    logger.Info("server config", "config", sConfig)
    assert.NotNil(sConfig, "unable to get config by id", "id", sId)
    return sId, sConfig
}

type ConnMap map[string][]*dummy.DummyClient

func hydrateServers(ctx context.Context, server *ServerState, logger *slog.Logger) ConnMap {
    configs, err := server.Sqlite.GetAllGameServerConfigs()
    assert.NoError(err, "unable to get game server configs")

    connMap := make(ConnMap)
    logger.Info("Hydrating Servers", "count", len(configs))
    for _, c := range configs {

        logger.Info("Creating server with the following config", "config", c)

        sId, sConfig := createServer(ctx, server, logger)
        factory := server.Factory.WithPort(uint16(sConfig.Port))
        conns := factory.CreateBatchedConnections(c.Connections)

        connMap[sId] = conns
    }

    return connMap
}

func copyFile(from string, to string) {
    toFd, err := os.OpenFile(to, os.O_RDWR|os.O_CREATE, 0644)
    assert.NoError(err, "unable to open toFile")
    defer toFd.Close()

    fromFd, err := os.Open(from)
    assert.NoError(err, "unable to open toFile")
    defer fromFd.Close()

    _, err = io.Copy(toFd, fromFd)
    assert.NoError(err, "unable to copy file")
}

func copyDBFile(path string) string {

    f, err := os.CreateTemp("/tmp", "mm-testing-")
    assert.NoError(err, "unable to create tmp")
    fName := f.Name()
    f.Close()

    copyFile(path, fName)
    copyFile(path + "-shm", fName + "-shm")
    copyFile(path + "-wal", fName + "-wal")

    return fName
}

func GetDBPath(name string) string {
    cwd, err := os.Getwd()
    assert.NoError(err, "no cwd?")

    // assert: windows sucks
    return path.Join(cwd, "data", name)
}

func CreateEnvironment(ctx context.Context, path string, params servermanagement.ServerParams) ServerState {
    logger := slog.Default().With("area", "create-env")
    logger.Warn("copying db file", "path", path)
    path = copyDBFile(path)
    os.Setenv("SQLITE", path)

    port, err := dummy.GetFreePort()
    assert.NoError(err, "unable to get a free port")

    logger.Info("creating sqlite", "path", path)
    sqlite := gameserverstats.NewSqlite(gameserverstats.EnsureSqliteURI(path))
    logger.Info("creating local servers", "params", params)
    local := servermanagement.NewLocalServers(sqlite, params)
    logger.Info("creating matchmaking", "port", port)

    mm := matchmaking.NewMatchMakingServer(matchmaking.MatchMakingServerParams{
        Port: port,
        GameServer: &local,
    })
    go mm.Run(ctx)
    mm.WaitForReady(ctx)

    logger.Info("creating client factory", "port", port)
    factory := NewTestingClientFactory("0.0.0.0", uint16(port), logger)

    logger.Info("creating server state object", "port", port)
    server := ServerState{
        Sqlite: sqlite,
        Server: &local,
        MatchMaking: mm,
        Port: port,
        Factory: &factory,
        Conns: nil,
    }

    logger.Info("hydrating servers", "port", port)
    server.Conns = hydrateServers(ctx, &server, logger)

    logger.Info("environment fully created")
    return server
}
