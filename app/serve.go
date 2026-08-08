package app

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"

	"daemonlord.ygg/madshare/api"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/webui"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Serve binds and serves one http.Server per [[listen]] entry, then one per
// enabled [[listen_mesh]] block. Each server runs in its own goroutine; a bind
// failure returns immediately, leaving already-started servers for Stop to close.
//
// It returns as soon as everything is listening — a server that later dies
// reports on Err() rather than taking the process with it, which is the one
// behaviour a library cannot inherit from a main().
func (i *Instance) Serve() error {
	cfg := i.cfg
	mesh := cfg.MeshListeners()
	if len(cfg.Listen)+len(mesh) == 0 {
		return ErrNoListeners
	}
	i.serveErr = make(chan error, len(cfg.Listen)+len(mesh))

	serve := func(addr string, groups, allow []string, ln net.Listener) error {
		handler, err := i.buildHandler(groups, allow)
		if err != nil {
			ln.Close()
			return err
		}
		srv := &http.Server{Addr: addr, Handler: handler}
		i.servers = append(i.servers, srv)
		i.log.Printf("listening on %s serving %v", addr, groups)
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				i.serveErr <- fmt.Errorf("serve %s: %w", srv.Addr, err)
			}
		}()
		return nil
	}

	for _, lc := range cfg.Listen {
		ln, err := net.Listen("tcp", lc.BindAddr())
		if err != nil {
			return err
		}
		if err := serve(lc.BindAddr(), lc.Serve, lc.AllowFrom, ln); err != nil {
			return err
		}
	}
	// Mesh listeners bind this node's own yggdrasil address over the federation
	// netstack rather than a kernel socket: no privileged-port rule (hence the
	// default of 80), and no reachability from this host (there is no TUN — see
	// federation.Mesh.ListenMesh). config.validateMesh has already refused a mesh
	// listener without a mesh, so a nil here would be a wiring bug, not a config
	// one.
	for _, mc := range mesh {
		if i.mesh == nil {
			return fmt.Errorf("listen_mesh[%d] port %d: the yggdrasil mesh is not running", mc.Index, mc.Port)
		}
		ln, err := i.mesh.ListenMesh(mc.Port)
		if err != nil {
			return err
		}
		addr := net.JoinHostPort(i.mesh.Address().String(), strconv.Itoa(mc.Port))
		if err := serve("mesh "+addr, mc.Serve, mc.AllowFrom, ln); err != nil {
			return err
		}
	}
	return nil
}

// Err reports a listener that died after Serve returned. It never closes and
// carries at most one error per listener, so a caller may select on it for the
// lifetime of the process. Nil before Serve is called.
func (i *Instance) Err() <-chan error { return i.serveErr }

// buildHandler composes the chi router for one listener: shared middleware plus
// only the route groups in serve. It takes the two per-listener values rather
// than a ListenConfig because a mesh listener ([[listen_mesh]]) has no bind
// address to give it — the transport differs, the handler does not.
func (i *Instance) buildHandler(serve, allow []string) (http.Handler, error) {
	web := i.cfg.WebUI
	serves := func(group string) bool { return slices.Contains(serve, group) }
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(api.CORS(i.cfg.CORS.AllowedOrigins))
	// Resolve the request's identity (session cookie / bearer token) for every
	// route; authorization is enforced per group below.
	if i.deps.Auth != nil {
		r.Use(auth.Identify(i.deps.Auth))
	}
	if len(allow) > 0 {
		mw, err := allowFrom(allow)
		if err != nil {
			return nil, err
		}
		r.Use(mw)
	}
	// Let GET routes answer HEAD. Registered last (innermost) so the rewrite to
	// GET happens just before routing: the logger above still records HEAD, and
	// Identify / allow_from have already run, so the HEAD takes the same auth and
	// access path as a GET. See api.SupportHEAD.
	r.Use(api.SupportHEAD)

	if serves(config.GroupAPI) {
		api.RegisterAPI(r, i.deps)
	}
	if serves(config.GroupAdmin) {
		// RegisterAdmin gates the destructive API by file.delete when auth is
		// configured (deps.Auth set). The admin page itself is left ungated so
		// it can render its login prompt.
		api.RegisterAdmin(r, i.deps)
		webui.RegisterAdminPage(r, web.APIBase, web.GitRepoURL()) // no-op in -tags nowebui builds
	}
	if serves(config.GroupWebUI) {
		// federated ([federation].enabled) decides whether the web UI's "/" front
		// door opens on the network or on the library.
		webui.Register(r, web.APIBase, web.GitRepoURL(), i.cfg.Federation.Enabled)
	}
	return r, nil
}

// allowFrom returns middleware that rejects (403) any request whose source IP
// is not within one of the given CIDRs. The CIDRs are already validated by
// config.Load; they are re-parsed here so the middleware owns its own state.
func allowFrom(cidrs []string) (func(http.Handler) http.Handler, error) {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, err
		}
		nets = append(nets, n)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			ip := net.ParseIP(host)
			for _, n := range nets {
				if ip != nil && n.Contains(ip) {
					next.ServeHTTP(w, r)
					return
				}
			}
			http.Error(w, "forbidden", http.StatusForbidden)
		})
	}, nil
}
