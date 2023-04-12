package registry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-logr/logr"
	"github.com/opencontainers/go-digest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	pkggin "github.com/xenitab/pkg/gin"

	"github.com/xenitab/spegel/internal/oci"
	"github.com/xenitab/spegel/internal/routing"
)

var mirrorRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "spegel_mirror_requests_total",
		Help: "Total number of mirror requests.",
	},
	[]string{"registry", "cache", "source"},
)

type Registry struct {
	srv *http.Server
}

func NewRegistry(ctx context.Context, addr string, ociClient oci.OCIClient, router routing.Router) (*Registry, error) {
	_, registryPort, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	log := logr.FromContextOrDiscard(ctx)
	cfg := pkggin.Config{
		LogConfig: pkggin.LogConfig{
			Logger:          log,
			PathFilter:      regexp.MustCompile("/healthz"),
			IncludeLatency:  true,
			IncludeClientIP: true,
			IncludeKeys:     []string{"handler"},
		},
		MetricsConfig: pkggin.MetricsConfig{
			HandlerID: "registry",
		},
	}
	engine := pkggin.NewEngine(cfg)
	registryHandler := &RegistryHandler{
		log:          log,
		ociClient:    ociClient,
		router:       router,
		registryPort: registryPort,
	}
	engine.GET("/healthz", registryHandler.readyHandler)
	engine.Any("/v2/*params", metricsHandler, registryHandler.registryHandler)
	srv := &http.Server{
		Addr:    addr,
		Handler: engine,
	}
	return &Registry{
		srv: srv,
	}, nil
}

func (r *Registry) ListenAndServe(ctx context.Context) error {
	if err := r.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (r *Registry) Shutdown() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.srv.Shutdown(shutdownCtx)
}

type RegistryHandler struct {
	log          logr.Logger
	ociClient    oci.OCIClient
	router       routing.Router
	registryPort string
}

func (r *RegistryHandler) readyHandler(c *gin.Context) {
	c.Status(http.StatusOK)
}

// TODO: Explore using leases to make sure resources are not deleted mid request.
// https://github.com/containerd/containerd/blob/main/docs/garbage-collection.md
func (r *RegistryHandler) registryHandler(c *gin.Context) {
	// Only deal with GET and HEAD requests.
	if !(c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) {
		c.Status(http.StatusNotFound)
		return
	}

	// Quickly return 200 for /v2/ to indicate that registry supports v2.
	if path.Clean(c.Request.URL.Path) == "/v2" {
		if c.Request.Method != http.MethodGet {
			c.Status(http.StatusNotFound)
			return
		}
		c.Status(http.StatusOK)
		return
	}

	// Always expect remoteRegistry header to be passed in request.
	remoteRegistry, err := getRemoteRegistry(c.Request.Header)
	if err != nil {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, err)
		return
	}

	// Request with mirror header are proxied.
	if isMirrorRequest(c.Request.Header) {
		r.handleMirror(c, remoteRegistry)
		return
	}

	// Serve registry endpoints.
	ref, ok, err := ManifestReference(remoteRegistry, c.Request.URL.Path)
	if err != nil {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, err)
		return
	}
	if ok {
		// TODO: Resolve tag
		r.handleManifest(c, ref)
		return
	}
	ref, ok, err = BlobReference(remoteRegistry, c.Request.URL.Path)
	if err != nil {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, err)
		return
	}
	if ok {
		// TODO: Require digest
		r.handleBlob(c, ref.Digest())
		return
	}

	// If nothing matches return 404.
	c.Status(http.StatusNotFound)
}

// TODO: Retry multiple endoints
func (r *RegistryHandler) handleMirror(c *gin.Context, remoteRegistry string) {
	c.Set("handler", "mirror")

	// Disable mirroring so we dont end with an infinite loop
	c.Request.Header[MirrorHeader] = []string{"false"}

	ref, ok, err := AnyReference(remoteRegistry, c.Request.URL.Path)
	if err != nil {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, err)
		return
	}
	if !ok {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, fmt.Errorf("could not parse reference"))
		return
	}

	// If digest is emtpy it means the ref is a tag
	key := ref.Digest().String()
	if key == "" {
		key = ref.String()
	}

	// We should allow resolving to ourself if the mirror request is external.
	isExternal := isExternalRequest(c.Request.Header)
	if isExternal {
		r.log.Info("handling mirror request from external node", "path", c.Request.URL.Path, "ip", c.RemoteIP())
	}

	// Resolve node with the requested key
	timeoutCtx, cancel := context.WithTimeout(c, 5*time.Second)
	defer cancel()
	ip, ok, err := r.router.Resolve(timeoutCtx, key, isExternal)
	if err != nil {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, err)
		return
	}
	if !ok {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, fmt.Errorf("could not find node with ref: %s", ref.String()))
		return
	}

	// Proxy the request to another registry
	url, err := url.Parse(fmt.Sprintf("http://%s:%s", ip, r.registryPort))
	if err != nil {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, err)
		return
	}
	r.log.V(5).Info("forwarding request", "path", c.Request.URL.Path, "url", url.String())
	proxy := httputil.NewSingleHostReverseProxy(url)
	proxy.ServeHTTP(c.Writer, c.Request)
}

func (r *RegistryHandler) handleManifest(c *gin.Context, dgst digest.Digest) {
	c.Set("handler", "manifest")

	b, mediaType, err := r.ociClient.GetContent(c, dgst)
	if err != nil {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, err)
		return
	}
	c.Header("Content-Type", mediaType)
	c.Header("Content-Length", strconv.FormatInt(int64(len(b)), 10))
	c.Header("Docker-Content-Digest", dgst.String())
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	_, err = c.Writer.Write(b)
	if err != nil {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, err)
		return
	}
	c.Status(http.StatusOK)
}

func (r *RegistryHandler) handleBlob(c *gin.Context, dgst digest.Digest) {
	c.Set("handler", "blob")

	size, err := r.ociClient.GetSize(c, dgst)
	if err != nil {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, err)
		return
	}
	c.Header("Content-Length", strconv.FormatInt(size, 10))
	c.Header("Docker-Content-Digest", dgst.String())
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	err = r.ociClient.Copy(c, dgst, c.Writer)
	if err != nil {
		//nolint:errcheck // ignore
		c.AbortWithError(http.StatusNotFound, err)
		return
	}
	c.Status(http.StatusOK)
}

func metricsHandler(c *gin.Context) {
	c.Next()
	handler, ok := c.Get("handler")
	if !ok {
		return
	}
	if handler != "mirror" {
		return
	}
	remoteRegistry, err := getRemoteRegistry(c.Request.Header)
	if err != nil {
		return
	}
	sourceType := "internal"
	if isExternalRequest(c.Request.Header) {
		sourceType = "external"
	}
	cacheType := "hit"
	if c.Writer.Status() != http.StatusOK {
		cacheType = "miss"
	}
	mirrorRequestsTotal.WithLabelValues(remoteRegistry, cacheType, sourceType).Inc()
}
