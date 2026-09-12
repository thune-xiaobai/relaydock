package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"relaydock/internal/channel"
	"relaydock/internal/config"
	"relaydock/internal/hub"
	"relaydock/internal/protocol"
	"relaydock/internal/wecom"
	"relaydock/internal/worker"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: relaydock init|hub|worker|chat|channel|wecom-inspect|wecom-resolve|wecom-baseline [options]")
	}
	mode := os.Args[1]
	if mode == "init" {
		return initConfig(os.Args[2:])
	}
	f := flag.NewFlagSet(mode, flag.ContinueOnError)
	file := f.String("config", "", "JSON configuration path")
	pid := f.Int("pid", 0, "native window process ID for inspection")
	rootRef := f.String("root-ref", "", "native window root ref for read-only inspection")
	outputID := f.String("output", "", "output ID to resolve")
	delivery := f.String("delivery", "", "sent or retry (explicit delivery resolution)")
	launch := f.String("launch", "", "internal launch file")
	if err := f.Parse(os.Args[2:]); err != nil {
		return err
	}
	if mode == "_spawn" {
		return worker.Spawn(*launch)
	}
	if mode != "hub" && mode != "worker" && mode != "chat" && mode != "channel" && mode != "wecom-inspect" && mode != "wecom-resolve" && mode != "wecom-baseline" {
		return errors.New("unknown command")
	}
	if *file == "" {
		return errors.New("--config is required")
	}
	c, err := config.Load(*file)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	switch mode {
	case "wecom-inspect":
		if c.WeCom == nil {
			return errors.New("wecom configuration is required")
		}
		helper, err := wecom.StartHelper(ctx, c.WeCom.Helper)
		if err != nil {
			return err
		}
		defer helper.Close()
		value, err := wecom.Inspect(ctx, helper, *pid, *rootRef, c.WeCom.Row)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	case "wecom-resolve":
		db, err := wecom.OpenState(c.StateDir)
		if err != nil {
			return err
		}
		defer db.Close()
		return wecom.Resolve(db, *outputID, *delivery)
	case "wecom-baseline":
		if c.WeCom == nil {
			return errors.New("wecom configuration is required")
		}
		helper, err := wecom.StartHelper(ctx, c.WeCom.Helper)
		if err != nil {
			return err
		}
		defer helper.Close()
		desktop, err := wecom.NewWindows(helper, *c.WeCom)
		if err != nil {
			return err
		}
		g, err := wecom.New(c, desktop, nil)
		if err != nil {
			return err
		}
		defer g.Close()
		if err = g.Baseline(ctx); err != nil {
			return err
		}
		fmt.Println("Visible history was skipped. Restart channel, then send new requests.")
		return nil
	case "hub":
		h, err := hub.New(c, nil)
		if err != nil {
			return err
		}
		defer h.Close()
		done := make(chan struct{})
		go func() { defer close(done); h.Serve(ctx) }()
		server := &http.Server{Addr: c.Listen, Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = server.Shutdown(shutdown)
		}()
		log.Printf("hub listening on %s", c.Listen)
		if c.Cert != "" {
			err = server.ListenAndServeTLS(c.Cert, c.Key)
		} else {
			err = server.ListenAndServe()
		}
		cancel()
		<-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case "worker":
		w, err := worker.New(c)
		if err != nil {
			return err
		}
		defer w.Close()
		return w.Run(ctx)
	case "chat", "channel":
		if mode == "channel" && c.Spool == "" && c.WeCom == nil {
			return errors.New("channel requires wecom configuration or spool directory")
		}
		if mode == "chat" {
			c.Spool = ""
			c.WeCom = nil
		}
		if c.WeCom != nil && c.Spool != "" {
			return errors.New("choose wecom or spool, not both")
		}
		ch, err := channel.New(c)
		if err != nil {
			return err
		}
		defer ch.Close()
		if mode == "chat" {
			fmt.Println("RelayDock：输入自然语言消息，Ctrl+C 退出。关闭本窗口不会关闭远端 pi。")
			ch.Console(os.Stdin, os.Stdout)
		}
		if c.WeCom != nil {
			helper, err := wecom.StartHelper(ctx, c.WeCom.Helper)
			if err != nil {
				return err
			}
			defer helper.Close()
			desktop, err := wecom.NewWindows(helper, *c.WeCom)
			if err != nil {
				return err
			}
			gateway, err := wecom.New(c, desktop, ch.Enqueue)
			if err != nil {
				return err
			}
			defer gateway.Close()
			ch.Output = gateway.EnqueueOutput
			child, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { defer close(done); gateway.Run(child) }()
			defer func() { stop(); <-done }()
			return ch.Run(child)
		}
		return ch.Run(ctx)
	}
	return nil
}

func initConfig(args []string) error {
	f := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := f.String("dir", ".relaydock", "new configuration directory")
	workspace := f.String("workspace", ".", "existing pi workspace")
	bridge := f.String("bridge", "extensions/relaydock.ts", "pi bridge extension")
	modelURL := f.String("model-url", "", "OpenAI-compatible base URL, e.g. http://host:8000/v1")
	modelName := f.String("model", "", "Hub tool-calling model name")
	if err := f.Parse(args); err != nil {
		return err
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		return errors.New("init requires a new directory; existing files are never overwritten")
	}
	work, err := filepath.Abs(*workspace)
	if err != nil {
		return err
	}
	ext, err := filepath.Abs(*bridge)
	if err != nil {
		return err
	}
	workerToken, channelToken := protocol.ID(""), protocol.ID("")
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	cfg := map[string]config.Config{
		"hub.json":     {StateDir: "hub-state", Listen: "127.0.0.1:7331", Workers: map[string]config.Peer{"local": {Token: workerToken}}, Channels: map[string]config.Peer{"console": {Token: channelToken, Nodes: []string{"local"}}}, Model: config.Model{URL: *modelURL, Model: *modelName}},
		"worker.json":  {StateDir: "worker-state", ID: "local", Name: "本机", Hub: "ws://127.0.0.1:7331/ws", Token: workerToken, Backend: "tmux", Namespace: "relaydock", Bridge: ext, Workspaces: map[string]string{"project": work}, Agents: map[string]config.Agent{"pi": {Executable: "pi"}}, MaxRunning: 4},
		"channel.json": {StateDir: "channel-state", ID: "console", Hub: "ws://127.0.0.1:7331/ws", Token: channelToken, Spool: "spool"},
	}
	for name, c := range cfg {
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(root, name), append(b, '\n'), 0600); err != nil {
			return err
		}
	}
	fmt.Printf("Created local configs in %s\nConfigure hub.json model (and env:MODEL_KEY if needed), then start hub, worker and chat with --config.\nWindows: set worker backend/mux to psmux. LAN: configure Hub TLS and use wss.\n", root)
	return nil
}
