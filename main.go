package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/aimjel/minecraft/chat"
	"github.com/pelletier/go-toml/v2"

	"github.com/dynamitemc/dynamite/core_commands"
	"github.com/dynamitemc/dynamite/logger"
	"github.com/dynamitemc/dynamite/server"
	"github.com/dynamitemc/dynamite/server/commands"
	"github.com/dynamitemc/dynamite/util"
	"github.com/dynamitemc/dynamite/web"
)

var log = logger.New()
var startTime = time.Now()

func startProfile() {
	file, _ := os.Create("cpu.out")
	pprof.StartCPUProfile(file)
}

func stopProfile() {
	pprof.StopCPUProfile()
	file, _ := os.Create("ram.out")
	runtime.GC()
	pprof.WriteHeapProfile(file)
	file.Close()
}

func start(cfg *server.Config) {
	srv, err := server.Listen(cfg, cfg.ServerIP+":"+strconv.Itoa(cfg.ServerPort), log, core_commands.Commands)
	log.Info("Opened TCP server on %s:%d", cfg.ServerIP, cfg.ServerPort)
	if err != nil {
		log.Error("Failed to open TCP server: %s", err)
		os.Exit(1)
	}
	log.Info("Done! (%v)", time.Since(startTime))
	//c := make(chan os.Signal, 1)
	//signal.Notify(c, os.Interrupt)

	/*go func() {
		<-c
		if util.HasArg("-prof") {
			stopProfile()
		}
		fmt.Print("\r> ")
		srv.ConsoleCommand("stop")
	}()*/

	go scanConsole(srv)
	err = srv.Start()
	if err != nil {
		log.Error("Failed to start server: %s", err)
		os.Exit(1)
	}
}

var cfg server.Config

func main() {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		panic(err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	log.Info("Starting Dynamite 1.20.1 server")
	if util.HasArg("-prof") {
		log.Info("Starting CPU/RAM profiler")
		startProfile()
	}

	if err := server.LoadConfig("config.toml", &cfg); err != nil {
		log.Info("%v loading config.toml. Using default config", err)
		cfg = server.DefaultConfig

		f, _ := os.OpenFile("config.toml", os.O_RDWR|os.O_CREATE, 0666)
		toml.NewEncoder(f).Encode(cfg)
	}
	log.Debug("Loaded config")

	if !cfg.Online && !util.HasArg("-no_offline_warn") {
		log.Warn("Offline mode is insecure and you should not use it unless for a private server.\nRead https://github.com/DynamiteMC/Dynamite/wiki/Why-you-shouldn't-use-offline-mode")
	}

	if cfg.Web.Enable {
		if !util.HasArg("-nogui") {
			go web.LaunchWebPanel(fmt.Sprintf("%s:%d", cfg.Web.ServerIP, cfg.Web.ServerPort), cfg.Web.Password, log)
		} else {
			log.Warn("Remove the -nogui argument to load the web panel")
		}
	}
	start(&cfg)
}

// The extremely fancy custom terminal thing
func scanConsole(srv *server.Server) {
	var command string
	for {
		var b [1]byte
		os.Stdin.Read(b[:])

		switch b[0] {
		case 127, 8: // backspace - delete character
			if len(command) > 0 {
				fmt.Print("\b \b")
				command = command[:len(command)-1]
				args := strings.Split(command, " ")

				cmd := srv.GetCommandGraph().FindCommand(args[0])
				if cmd == nil {
					fmt.Printf("\r> %s", logger.R(command))
				} else {
					if len(args) > 1 {
						fmt.Printf("\r> %s %s", args[0], logger.C(strings.Join(args[1:], " ")))
					} else {
						fmt.Printf("\r> %s", args[0])
					}
				}
			}
		case 3: // ctrl c - stop the server
			fmt.Print("\r")
			if len(command) > len("stop") {
				fmt.Print("\x1b[K")
			}
			fmt.Print("> ")
			srv.ConsoleCommand("stop")
		case 13: // enter - run the command and clear it
			fmt.Print("\r> \n\r")
			command = strings.TrimSpace(command)
			args := strings.Split(command, " ")

			cmd := srv.GetCommandGraph().FindCommand(args[0])
			if cmd == nil {
				srv.Logger.Print(chat.NewMessage(fmt.Sprintf("&cUnknown or incomplete command, see below for error\n\r&n%s&r&c&o<--[HERE]", args[0])))
				command = ""
				continue
			}
			cmd.Execute(commands.CommandContext{
				Executor:    &server.ConsoleExecutor{Server: srv},
				Arguments:   args[1:],
				FullCommand: command,
			})
			command = ""
		default: // regular character - add to current command input
			command += string(b[0])
			args := strings.Split(command, " ")

			cmd := srv.GetCommandGraph().FindCommand(args[0])
			if cmd == nil {
				fmt.Printf("\r> %s", logger.R(command))
			} else {
				if len(args) > 1 {
					fmt.Printf("\r> %s %s", args[0], logger.C(strings.Join(args[1:], " ")))
				} else {
					fmt.Printf("\r> %s", args[0])
				}
			}
		}
	}
}
