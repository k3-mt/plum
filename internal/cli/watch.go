package cli

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/k3-mt/plum/internal/bundle"
	"github.com/k3-mt/plum/internal/claims"
	"github.com/k3-mt/plum/internal/explore"
	"github.com/k3-mt/plum/internal/lang/dbt"
	"github.com/k3-mt/plum/internal/met"
	"github.com/k3-mt/plum/internal/server"
	"github.com/k3-mt/plum/internal/store"
	"github.com/k3-mt/plum/internal/trace"
)

// `plum explore` opens one recording and is finished when you close it. `plum
// watch` is the same picture with a different lifetime: it starts before there
// is anything to look at, follows each session as the agent produces it, and is
// still there tomorrow. That difference is the whole command — a window you can
// leave on a second screen and glance at, rather than a page you go and fetch.
func cmdWatch(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	addr := fs.String("addr", "", "listen address (default: a stable port for this repository)")
	noOpen := fs.Bool("no-open", false, "do not open a window")
	tab := fs.Bool("tab", false, "open an ordinary browser tab rather than an application window")
	noFollow := fs.Bool("no-follow", false, "stay on the session it started with")
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	bind := *addr
	if bind == "" {
		bind = fmt.Sprintf("127.0.0.1:%d", stablePort(env.Cfg.Root))
	}
	profile := filepath.Join(store.StateDir(env.Cfg.Root), "window")

	// One window per repository. A second `plum watch` should raise the window
	// that is already running, the way opening a project twice does in an editor,
	// rather than starting a rival server on a random port and leaving two views
	// disagreeing about which session is current.
	if running, id := probe(ctx, bind, env.Cfg.Root); running {
		fmt.Println("plum watch is already running for this repository →", "http://"+bind)
		if id != "" {
			fmt.Println("showing session", id)
		}
		if !*noOpen {
			server.OpenWindow("http://"+bind, profile)
		}
		return nil
	}

	b, l, events, cs, synthesis, id := latestSession(ctx, env, fs.Args())
	opts := server.Config{
		JournalDir: env.Cfg.Repo.JournalDir,
		Adapters:   env.Reg,
		Watch:      true,
		Sessions:   env.Store,
		Follow:     !*noFollow,
		Resident:   true,
		Window:     !*tab,
		ProfileDir: profile,
		Met:        met.Load(store.StateDir(env.Cfg.Root)),
	}
	if id != "" {
		opts.SessionDir = env.Store.Dir(id)
		opts.ClaimsPath = env.Store.ClaimsPath(id)
		if flow, err := dbt.LoadFlow(env.Store.FlowPath(id)); err == nil {
			opts.Flow = flow
		}
	} else {
		fmt.Println("no sessions recorded yet — the window will pick up the first one that is.")
		fmt.Println("`plum hooks install` is what makes that happen without you asking.")
	}
	routeAsk(ctx, env, &opts)

	tel := explore.NewStore(store.StateDir(env.Cfg.Root))
	s := server.New(env.Cfg, b, l, events, cs, synthesis, tel, opts)

	err := s.Serve(ctx, bind, !*noOpen)
	// The stable port is a convenience, not a requirement. Something else
	// holding it must not be the difference between having the window and not.
	if isAddrInUse(err) && *addr == "" {
		fmt.Printf("port %s is taken by something that is not plum; using a temporary one\n", bind)
		return s.Serve(ctx, "127.0.0.1:0", !*noOpen)
	}
	return err
}

// latestSession loads the newest recording, or an empty one when the repository
// has none yet. A window that refuses to start until there is something to show
// is a window you have to remember to start, which is the habit this is trying
// to remove.
func latestSession(ctx context.Context, env *Env, args []string) (*bundle.Bundle, trace.Landscape, []trace.Event, []claims.Claim, string, string) {
	empty := &bundle.Bundle{}
	id, err := env.Store.ResolveRef(ctx, env.Repo, first(args))
	if err != nil {
		return empty, trace.Landscape{}, nil, nil, "", ""
	}
	b, err := env.Store.Load(id)
	if err != nil {
		return empty, trace.Landscape{}, nil, nil, "", ""
	}
	var l trace.Landscape
	if got, err := loadLandscape(env, id); err == nil {
		l = *got
	}
	events, _ := trace.ReadFile(env.Store.TracePath(id))
	cs, _ := claims.Load(env.Store.ClaimsPath(id))
	synthesis, _ := os.ReadFile(env.Store.SynthesisPath(id))
	return b, l, events, cs, string(synthesis), id
}

// stablePort derives the address from the repository path, so the window keeps
// one URL for the life of the repository. A bookmark, a docked editor pane and a
// window reopened next week all come back to the same place — which they cannot
// do if the port is whatever was free at the time.
//
// The range sits below the ephemeral ports every platform allocates from, so the
// operating system will not hand this port to an outgoing connection first.
func stablePort(root string) int {
	sum := sha256.Sum256([]byte("plum-watch\x00" + root))
	return 17000 + int(binary.BigEndian.Uint16(sum[:2])%1000)
}

// probe asks whoever holds the port whether they are plum, and plum for this
// repository. The port is a hash of a path, and a collision must not quietly
// attach this window to a different codebase.
func probe(ctx context.Context, addr, root string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/api/health", nil)
	if err != nil {
		return false, ""
	}
	client := &http.Client{Timeout: 400 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	var health struct {
		Plum    string `json:"plum"`
		Repo    string `json:"repo"`
		Session string `json:"session"`
	}
	if json.NewDecoder(resp.Body).Decode(&health) != nil {
		return false, ""
	}
	if health.Plum != "ok" || health.Repo != root {
		return false, ""
	}
	return true, health.Session
}

func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return true
	}
	return strings.Contains(err.Error(), "address already in use")
}
