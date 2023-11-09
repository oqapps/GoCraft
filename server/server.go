package server

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dynamitemc/dynamite/server/config"
	"github.com/dynamitemc/dynamite/server/controller"
	"github.com/dynamitemc/dynamite/server/entity"
	"github.com/dynamitemc/dynamite/server/handler"
	"github.com/dynamitemc/dynamite/server/lang"
	"github.com/dynamitemc/dynamite/server/permission"
	"golang.org/x/term"

	"github.com/aimjel/minecraft/chat"
	"github.com/aimjel/minecraft/protocol/types"
	"github.com/pelletier/go-toml/v2"

	"github.com/google/uuid"

	"github.com/aimjel/minecraft"
	"github.com/aimjel/minecraft/packet"

	//"github.com/dynamitemc/dynamite/web"
	"github.com/dynamitemc/dynamite/logger"
	"github.com/dynamitemc/dynamite/server/commands"
	"github.com/dynamitemc/dynamite/server/player"
	"github.com/dynamitemc/dynamite/server/world"
	"github.com/dynamitemc/dynamite/web"
)

func New(cfg *config.Config, address string, logger *logger.Logger, commandGraph *commands.Graph) (*Server, error) {
	lnCfg := minecraft.ListenConfig{
		Status: minecraft.NewStatus(minecraft.Version{
			Text:     "DynamiteMC 1.20.1",
			Protocol: 763,
		}, cfg.MaxPlayers, cfg.MOTD, true, true),
		OnlineMode:           cfg.Online,
		CompressionThreshold: int32(cfg.CompressionThreshold),
	}

	if cfg.Chat.Secure && !cfg.Online {
		logger.Warn("Secure chat doesn't work on offline mode")
		cfg.Chat.Secure = false
	}
	if cfg.Chat.Secure && cfg.Chat.Format != "" {
		logger.Warn("Secure chat overrides the chat format")
	}
	if cfg.TPS < 20 {
		logger.Warn("TPS must be at least 20")
		cfg.TPS = 20
	}
	if cfg.ResourcePack.Enable && cfg.ResourcePack.URL == "" {
		logger.Warn("Resource pack is enabled but no url is provided")
		cfg.ResourcePack.Enable = false
	}

	//web.SetMaxPlayers(cfg.MaxPlayers)

	ln, err := lnCfg.Listen(address)
	if err != nil {
		return nil, err
	}
	w, err := world.OpenWorld("world", cfg.Superflat)
	if err != nil {
		world.CreateWorld(cfg.Hardcore)
		logger.Error("Failed to load world: %s", err)
		os.Exit(1)
	}
	w.Gamemode = byte(player.Gamemode(cfg.Gamemode))
	srv := &Server{
		Config:       cfg,
		listener:     ln,
		Logger:       logger,
		World:        w,
		Players:      controller.New[uuid.UUID, *player.Player](),
		Entities:     controller.New[int32, entity.Entity](),
		commandGraph: commandGraph,
		Lang:         lang.New("lang.json"),
	}

	logger.Info("Loading player info")
	srv.loadFiles()

	go srv.tickLoop()

	return srv, nil
}

var idCounter atomic.Int32

type Server struct {
	Config       *config.Config
	Logger       *logger.Logger
	commandGraph *commands.Graph

	Lang *lang.Lang

	WhitelistedPlayers,
	Operators,
	BannedPlayers,
	BannedIPs []user

	listener *minecraft.Listener

	Players  *controller.Controller[uuid.UUID, *player.Player]
	Entities *controller.Controller[int32, entity.Entity]

	World *world.World

	mu sync.RWMutex
}

func (srv *Server) Start() error {
	for {
		conn, err := srv.listener.Accept()
		if err != nil {
			return err
		}
		go srv.handleNewConn(conn)
	}
}

func (srv *Server) NewID() int32 {
	return idCounter.Add(1)
}

func (srv *Server) handleNewConn(conn *minecraft.Conn) {
	if srv.ValidateConn(conn) {
		conn.Close(nil)
		return
	}

	x := conn.UUID()
	uuid, _ := uuid.FromBytes(x[:])

	data := srv.World.GetPlayerData(uuid.String())

	plyr := player.New(
		uuid,
		srv.Players,
		srv.Entities,
		srv,
		srv.Config,
		srv.Lang,
		srv.Logger,
		idCounter.Add(1),
		conn,
		data,
		srv.World.GetDimension(data.Dimension),
		int8(srv.Config.ViewDistance),
		srv.NewID,
	)

	for _, op := range srv.Operators {
		if op.UUID == uuid.String() {
			plyr.SetOperator(true)
		}
	}

	plyr.SendCommands(srv.commandGraph)

	//cntrl.SendPacket(&packet.SetTablistHeaderFooter{
	//	Header: strings.Join(srv.Config.Tablist.Header, "\n"),
	//	Footer: strings.Join(srv.Config.Tablist.Footer, "\n"),
	//})

	srv.addPlayer(plyr)
	plyr.Login(plyr.Dimension())

	plyr.IntitializeData()

	if err := srv.handlePackets(plyr); err != nil {
		srv.Players.Delete(plyr.UUID())
		prefix, suffix := plyr.GetPrefixSuffix()
		srv.Logger.Info("[%s] Player %s (%s) has left the server", conn.RemoteAddr().String(), conn.Name(), plyr.UUID())

		srv.GlobalMessage(srv.Lang.Translate("player.leave", map[string]string{"player": plyr.Name(), "player_prefix": prefix, "player_suffix": suffix}))
		srv.playerlistRemove(conn.UUID())
		plyr.Despawn()
		plyr.Save()

		//todo consider moving logic of removing player to a separate function
	}
}

func (srv *Server) addPlayer(p *player.Player) {
	p.SendPacket(&packet.ServerData{
		MOTD:               chat.NewMessage(srv.Config.MOTD),
		EnforcesSecureChat: true,
	})
	newPlayer := types.PlayerInfo{
		UUID:       p.UUID(),
		Name:       p.Name(),
		Properties: p.Properties(),
		Listed:     true,
	}

	srv.Players.Set(p.UUID(), p)
	players := make([]types.PlayerInfo, 0, srv.Players.Count())

	srv.Players.Range(func(_ uuid.UUID, pl *player.Player) bool {
		id, pk, ks, ex := pl.SessionID()
		players = append(players, types.PlayerInfo{
			UUID:          pl.UUID(),
			Name:          pl.Name(),
			Properties:    pl.Properties(),
			Listed:        true,
			PublicKey:     bytes.Clone(pk),
			KeySignature:  bytes.Clone(ks),
			ChatSessionID: id,
			ExpiresAt:     ex,
		})
		if pl.UUID() != p.UUID() {
			pl.SendPacket(&packet.PlayerInfoUpdate{
				Actions: 0x01 | 0x08,
				Players: []types.PlayerInfo{newPlayer},
			})
		}
		return true
	})

	//updates the new session's player list
	var actions byte = 0x01 | 0x08
	if srv.Config.Chat.Secure {
		actions |= 0x02
	}
	p.SendPacket(&packet.PlayerInfoUpdate{
		Actions: actions,
		Players: players,
	})

	if srv.Config.Web.Enable {
		web.AddPlayer(p.Name(), p.UUID().String())
	}
	prefix, suffix := p.GetPrefixSuffix()

	srv.Logger.Info("[%s] Player %s (%s) has joined the server", p.IP(), p.Name(), p.UUID())

	srv.GlobalMessage(srv.Lang.Translate("player.join", map[string]string{"player": p.Name(), "player_prefix": prefix, "player_suffix": suffix}))
}

func (srv *Server) handlePackets(p *player.Player) error {
	ticker := time.NewTicker(25 * time.Second)
	for {
		select {
		case <-ticker.C:
			p.Keepalive()
		default:
		}

		packt, err := p.ReadPacket()
		if err != nil {
			return err
		}

		switch pk := packt.(type) {
		case *packet.PlayerCommandServer:
			handler.PlayerCommand(p, pk.ActionID)
		case *packet.ChatMessageServer:
			handler.ChatMessagePacket(p, pk)
		case *packet.ChatCommandServer:
			handler.ChatCommandPacket(p, srv.commandGraph, srv.Logger, pk.Command, pk.Timestamp, pk.Salt, pk.ArgumentSignatures)
		case *packet.ClientSettings:
			handler.ClientSettings(p, pk)
		case *packet.PlayerPosition, *packet.PlayerPositionRotation, *packet.PlayerRotation:
			handler.PlayerMovement(p, pk)
		case *packet.PlayerActionServer:
			handler.PlayerAction(p, pk)
		case *packet.InteractServer:
			handler.Interact(p, pk)
		case *packet.SwingArmServer:
			handler.SwingArm(p, pk.Hand)
		case *packet.CommandSuggestionsRequest:
			handler.CommandSuggestionsRequest(pk.TransactionId, pk.Text, srv.commandGraph, p)
		case *packet.ClientCommandServer:
			handler.ClientCommand(p, pk.ActionID)
		case *packet.PlayerAbilitiesServer:
			handler.PlayerAbilities(p, pk.Flags)
		case *packet.PlayerSessionServer:
			p.SetSessionID(pk.SessionID, pk.PublicKey, pk.KeySignature, pk.ExpiresAt)
		case *packet.SetHeldItemServer:
			handler.SetHeldItem(p, pk.Slot)
		case *packet.SetCreativeModeSlot:
			handler.SetCreativeModeSlot(p, pk.Slot, pk.ClickedItem)
		case *packet.TeleportToEntityServer:
			handler.TeleportToEntity(p, pk.Player)
		case *packet.ClickContainer:
			handler.ClickContainer(p, pk)
		}
	}
}

func (srv *Server) GetCommandGraph() *commands.Graph {
	return srv.commandGraph
}

func (srv *Server) Reload() error {
	srv.loadFiles()

	config.LoadConfig("config.toml", srv.Config)

	permission.Clear()

	srv.Players.Range(func(_ uuid.UUID, p *player.Player) bool {
		if srv.Config.Whitelist.Enforce && srv.Config.Whitelist.Enable && !srv.IsWhitelisted(p.UUID()) {
			p.Disconnect(srv.Lang.Translate("disconnect.not_whitelisted", nil))
			return true
		}

		p.SetOperator(srv.IsOperator(p.UUID()))
		p.SendCommands(srv.commandGraph)
		return true
	})
	return nil
}

func (srv *Server) FindPlayer(username string) *player.Player {
	_, p := srv.Players.Range(func(_ uuid.UUID, p *player.Player) bool {
		return !strings.EqualFold(p.Name(), username)
	})

	return p
}

func (srv *Server) FindPlayerByID(id int32) *player.Player {
	_, p := srv.Players.Range(func(_ uuid.UUID, p *player.Player) bool {
		return p.EntityID() != id
	})

	return p
}

func (srv *Server) Close() {
	srv.Logger.Info("Stopping server")

	srv.Logger.Info("Saving player data")
	var files = []string{"whitelist.json", "banned_players.json", "ops.json", "banned_ips.json"}
	var lists = [][]user{srv.WhitelistedPlayers, srv.BannedPlayers, srv.Operators, srv.BannedIPs}
	for i, file := range files {
		WritePlayerList(file, lists[i])
	}

	permission.Save()

	srv.Players.Range(func(_ uuid.UUID, p *player.Player) bool {
		p.Disconnect(srv.Lang.Translate("disconnect.server_shutdown", nil))
		p.Save()
		return true
	})

	srv.Logger.Info("Saving world")
	srv.World.Save()

	f, _ := os.OpenFile("config.toml", os.O_RDWR|os.O_CREATE, 0666)
	toml.NewEncoder(f).Encode(srv.Config)
	term.Restore(int(os.Stdin.Fd()), OldState)
	os.Exit(0)
}

func (srv *Server) loadFiles() {
	var files = []string{"whitelist.json", "banned_players.json", "ops.json", "banned_ips.json"}
	var addresses = []*[]user{&srv.WhitelistedPlayers, &srv.BannedPlayers, &srv.Operators, &srv.BannedIPs}
	for i, file := range files {
		u, err := loadUsers(file)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			srv.Logger.Warn("%v loading %v", err, file)
		}

		*addresses[i] = u
	}
}

func (srv *Server) ConsoleCommand(txt string) {
	fmt.Println(txt)
	fmt.Print("\r")
	content := strings.TrimSpace(txt)
	args := strings.Split(content, " ")

	command := srv.GetCommandGraph().FindCommand(args[0])
	if command == nil {
		srv.Logger.Print(chat.NewMessage(fmt.Sprintf("&cUnknown or incomplete command, see below for error\n&n%s&r&c&o<--[HERE]", args[0])))
		return
	}
	command.Execute(commands.CommandContext{
		Command:     command,
		Arguments:   args[1:],
		Executor:    srv,
		FullCommand: content,
	})
}

var OldState *term.State
