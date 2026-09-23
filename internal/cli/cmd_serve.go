package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sherpa/internal/compare"
	"sherpa/internal/state"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui/*
var ui embed.FS

func init() { register("serve", cmdServe) }

type localServer struct {
	home, token, host string
	mu                sync.Mutex
	jobs              map[string]context.CancelFunc
	wg                sync.WaitGroup
	closing           bool
}

func cmdServe(ctx *Ctx, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(ctx.Stderr)
	port := flags.Int("port", 7331, "local port; 0 chooses an available port")
	noOpen := flags.Bool("no-open", false, "print URL without opening a browser")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *port < 0 || *port > 65535 {
		return fmt.Errorf("usage: sherpa serve [--port 7331] [--no-open]")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	defer listener.Close()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	app := &localServer{home: ctx.Home, token: hex.EncodeToString(b), host: listener.Addr().String(), jobs: map[string]context.CancelFunc{}}
	server := &http.Server{Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second}
	url := "http://" + app.host + "/#" + app.token
	fmt.Fprintf(ctx.Stdout, "SherpA local workspace: %s\nPress Ctrl+C to stop.\n", url)
	if !*noOpen {
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("open", url)
		case "windows":
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
		default:
			cmd = exec.Command("xdg-open", url)
		}
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(ctx.Stderr, "Open the URL above in your browser.")
		} else {
			go cmd.Wait()
		}
	}
	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	go func() {
		<-stop.Done()
		app.stop()
		server.Close()
	}()
	err = server.Serve(listener)
	app.stop()
	cancel()
	app.wg.Wait()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
func (s *localServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if r.Host != s.host {
		http.Error(w, "invalid host", http.StatusForbidden)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", 405)
			return
		}
		assets, _ := fs.Sub(ui, "ui")
		http.FileServer(http.FS(assets)).ServeHTTP(w, r)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		http.Error(w, "Open the private URL printed by sherpa serve to connect.", 401)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+s.host {
		http.Error(w, "invalid origin", 403)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method == "GET" {
		s.read(w, r)
		return
	}
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "JSON required", 415)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		apiError(w, fmt.Errorf("server is stopping"), 503)
		return
	}
	unlock, err := state.Lock(s.home)
	if err != nil {
		apiError(w, err, 409)
		return
	}
	defer unlock()
	switch r.URL.Path {
	case "/api/compare":
		if len(s.jobs) > 0 {
			apiError(w, fmt.Errorf("a comparison is already running; cancel it or wait"), 409)
			return
		}
		var req compare.Request
		if !decode(w, r, &req) {
			return
		}
		c, err := compare.Prepare(s.home, req)
		if err != nil {
			apiError(w, err, 400)
			return
		}
		runCtx, cancel := context.WithCancel(context.Background())
		s.jobs[c.ID] = cancel
		s.wg.Add(1)
		// Encode before starting so the response and runner never share mutable data.
		json.NewEncoder(w).Encode(c)
		go func() {
			defer s.wg.Done()
			defer cancel()
			err := compare.Execute(runCtx, s.home, c)
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.jobs, c.ID)
			if err != nil {
				c.Status = "failed"
				for i := range c.Results {
					if c.Results[i].Status == "running" || c.Results[i].Status == "queued" {
						c.Results[i].Status = "failed"
						c.Results[i].Error = err.Error()
					}
				}
				compare.Save(s.home, c)
			}
		}()
	case "/api/cancel":
		var req struct {
			ID string `json:"id"`
		}
		if !decode(w, r, &req) {
			return
		}
		cancel, ok := s.jobs[req.ID]
		if !ok {
			apiError(w, fmt.Errorf("no running comparison with this ID in this server"), 404)
			return
		}
		cancel()
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case "/api/rate":
		var req struct {
			ID      string `json:"id"`
			Profile string `json:"profile"`
			Score   int    `json:"score"`
			Notes   string `json:"notes"`
		}
		if !decode(w, r, &req) {
			return
		}
		if err := compare.Rate(s.home, req.ID, req.Profile, req.Score, req.Notes); err != nil {
			apiError(w, err, 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case "/api/action":
		var req struct {
			Action       string `json:"action"`
			Name         string `json:"name"`
			From         string `json:"from"`
			Path         string `json:"path"`
			Harness      string `json:"harness"`
			Instructions string `json:"instructions"`
			Trusted      bool   `json:"trusted"`
		}
		if !decode(w, r, &req) {
			return
		}
		var args []string
		var cmd command
		switch req.Action {
		case "init":
			cmd = cmdInit
			args = []string{"--primary-harness", req.Harness}
		case "use":
			cmd = cmdUse
			args = []string{req.Name}
		case "back":
			cmd = cmdBack
		case "create":
			cmd = cmdProfile
			args = []string{"create", req.Name, "--from", req.From, "--instructions", req.Instructions}
		case "import":
			cmd = cmdProfile
			args = []string{"import", req.Name, "--path", req.Path, "--harness", req.Harness}
			if req.Trusted {
				args = append(args, "--trusted")
			}
		default:
			apiError(w, fmt.Errorf("unsupported action"), 400)
			return
		}
		var out, errout bytes.Buffer
		ctx := &Ctx{Home: s.home, Stdout: &out, Stderr: &errout, Stdin: strings.NewReader("")}
		if err := cmd(ctx, args); err != nil {
			apiError(w, err, 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"message": out.String()})
	default:
		http.NotFound(w, r)
	}
}
func (s *localServer) read(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/state":
		st, err := state.Load(s.home)
		if err != nil {
			apiError(w, err, 500)
			return
		}
		// Registry metadata/legacy journals never enter the local application's API.
		type setup struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			Harness  string `json:"harness"`
			Baseline bool   `json:"baseline"`
		}
		profiles := []setup{}
		for name, p := range st.Profiles {
			profiles = append(profiles, setup{name, p.Path, p.Harness, st.Baselines[p.Harness] == name})
		}
		detected := []string{}
		setups, _ := detectHarnessSetups()
		for _, p := range setups {
			detected = append(detected, p.harness.Name())
		}
		json.NewEncoder(w).Encode(map[string]any{"active": st.Active, "profiles": profiles, "detected": detected})
	case "/api/comparisons":
		list, err := compare.List(s.home)
		if err != nil {
			apiError(w, err, 500)
			return
		}
		json.NewEncoder(w).Encode(list)
	case "/api/comparison", "/api/report":
		c, err := compare.Load(s.home, r.URL.Query().Get("id"))
		if err != nil {
			apiError(w, err, 404)
			return
		}
		if r.URL.Path == "/api/report" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="sherpa-`+c.ID+`.html"`)
			compare.WriteReport(w, c)
			return
		}
		json.NewEncoder(w).Encode(c)
	default:
		http.NotFound(w, r)
	}
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		apiError(w, err, 400)
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		apiError(w, fmt.Errorf("expected one JSON object"), 400)
		return false
	}
	return true
}
func apiError(w http.ResponseWriter, err error, status int) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *localServer) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	for _, cancel := range s.jobs {
		cancel()
	}
}
