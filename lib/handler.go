package lib

import (
	"net/http"
	"os"
	"strings"

	"github.com/rs/cors"
	"go.uber.org/zap"
	"golang.org/x/net/webdav"
)

type handlerUser struct {
	User
	webdav.Handler
}

type Handler struct {
	noPassword  bool
	behindProxy bool
	user        *handlerUser
	users       map[string]*handlerUser
}

func NewHandler(c *Config) (http.Handler, error) {
	ls := webdav.NewMemLS()

	h := &Handler{
		noPassword:  c.NoPassword,
		behindProxy: c.BehindProxy,
		user: &handlerUser{
			User: User{
				UserPermissions: c.UserPermissions,
			},
			Handler: webdav.Handler{
				Prefix: c.Prefix,
				FileSystem: Dir{
					Dir:     webdav.Dir(c.Directory),
					noSniff: c.NoSniff,
				},
				LockSystem: &lockSystem{
					LockSystem: ls,
					directory:  c.Directory,
				},
			},
		},
		users: map[string]*handlerUser{},
	}

	for _, u := range c.Users {
		h.users[u.Username] = &handlerUser{
			User: u,
			Handler: webdav.Handler{
				Prefix: c.Prefix,
				FileSystem: Dir{
					Dir:     webdav.Dir(u.Directory),
					noSniff: c.NoSniff,
				},
				LockSystem: &lockSystem{
					LockSystem: ls,
					directory:  u.Directory,
				},
			},
		}
	}

	if c.CORS.Enabled {
		return cors.New(cors.Options{
			AllowCredentials:   c.CORS.Credentials,
			AllowedOrigins:     c.CORS.AllowedHosts,
			AllowedMethods:     c.CORS.AllowedMethods,
			AllowedHeaders:     c.CORS.AllowedHeaders,
			ExposedHeaders:     c.CORS.ExposedHeaders,
			OptionsPassthrough: false,
		}).Handler(h), nil
	}

	if len(c.Users) == 0 {
		zap.L().Warn("unprotected config: no users have been set, so no authentication will be used")
	}

	if c.NoPassword {
		zap.L().Warn("unprotected config: password check is disabled, only intended when delegating authentication to another service")
	}

	return h, nil
}

// ServeHTTP determines if the request is for this plugin, and if all prerequisites are met.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user := h.user

	// Authentication
	if len(h.users) > 0 {
		w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)

		// Retrieve the real client IP address using the updated helper function
		remoteAddr := getRealRemoteIP(r, h.behindProxy)

		// Gets the correct user for this request.
		username, password, ok := r.BasicAuth()
		if !ok {
			http.Error(w, "Not authorized", http.StatusUnauthorized)
			return
		}

		user, ok = h.users[username]
		if !ok {
			// Log invalid username
			zap.L().Info("invalid username", zap.String("username", username), zap.String("remote_address", remoteAddr))
			http.Error(w, "Not authorized", http.StatusUnauthorized)
			return
		}

		if !h.noPassword && !user.checkPassword(password) {
			// Log invalid password
			zap.L().Info("invalid password", zap.String("username", username), zap.String("remote_address", remoteAddr))
			http.Error(w, "Not authorized", http.StatusUnauthorized)
			return
		}

		// Log successful authorization
		zap.L().Info("user authorized", zap.String("username", username), zap.String("remote_address", remoteAddr))
	}

	// Convert the HTTP request into an internal request type
	req, err := newRequest(r, h.user.Prefix)
	if err != nil {
		zap.L().Info("invalid request path or destination", zap.Error(err))
		http.Error(w, "Invalid request path or destination", http.StatusBadRequest)
		return
	}

	// Checks for user permissions relatively to this PATH.
	allowed := user.Allowed(req, func(filename string) bool {
		_, err := user.FileSystem.Stat(r.Context(), filename)
		return !os.IsNotExist(err)
	})

	zap.L().Debug("allowed & method & path", zap.Bool("allowed", allowed), zap.String("method", r.Method), zap.String("path", r.URL.Path))

	if !allowed {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.Method == "HEAD" {
		w = responseWriterNoBody{w}
	}

	// Excerpt from RFC4918, section 9.4:
	//
	// 		GET, when applied to a collection, may return the contents of an
	//		"index.html" resource, a human-readable view of the contents of
	//		the collection, or something else altogether.
	//
	//    Similarly, since the definition of HEAD is a GET without a response
	// 		message body, the semantics of HEAD are unmodified when applied to
	// 		collection resources.
	//
	// GET (or HEAD), when applied to collection, will return the same as PROPFIND method.
	if (r.Method == "GET" || r.Method == "HEAD") && strings.HasPrefix(r.URL.Path, user.Prefix) {
		info, err := user.FileSystem.Stat(r.Context(), strings.TrimPrefix(r.URL.Path, user.Prefix))
		if err == nil && info.IsDir() {
			r.Method = "PROPFIND"

			if r.Header.Get("Depth") == "" {
				r.Header.Add("Depth", "1")
			}
		}
	}

	// Runs the WebDAV.
	user.ServeHTTP(w, r)
}

// getRealRemoteIP retrieves the client's actual IP address, considering reverse proxies.
func getRealRemoteIP(r *http.Request, behindProxy bool) string {
	if behindProxy {
		if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
			return ip
		}
	}
	return r.RemoteAddr
}

type responseWriterNoBody struct {
	http.ResponseWriter
}

func (w responseWriterNoBody) Write(data []byte) (int, error) {
	return len(data), nil
}
