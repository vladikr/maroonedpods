package maroonedpods_server

import (
	"fmt"
	"github.com/rs/cors"
	"io"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/certificate"
	"k8s.io/klog/v2"
	"maroonedpods.io/pkg/util"
	"net/http"
)

const (
	healthzPath = "/healthz"
	ServePath   = "/serve-path"
)

// Server is the public interface to the upload proxy
type Server interface {
	Start() error
	ServeHTTP(w http.ResponseWriter, r *http.Request)
}

type MaroonedPodsServer struct {
	bindAddress       string
	bindPort          uint
	secretCertManager certificate.Manager
	handler           http.Handler
	maroonedpodsNS             string
}

// MaroonedPodsServer returns an initialized uploadProxyApp
func MaroonedPodsServer(maroonedpodsNS string,
	bindAddress string,
	bindPort uint,
	secretCertManager certificate.Manager,
	maroonedpodsCli kubernetes.Interface,
) (Server, error) {
	app := &MaroonedPodsServer{
		secretCertManager: secretCertManager,
		bindAddress:       bindAddress,
		bindPort:          bindPort,
		maroonedpodsNS:             maroonedpodsNS,
	}
	app.initHandler(maroonedpodsCli)

	return app, nil
}

func (app *MaroonedPodsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app.handler.ServeHTTP(w, r)
}

func (app *MaroonedPodsServer) initHandler(maroonedpodsCli kubernetes.Interface) {
	mux := http.NewServeMux()
	mux.HandleFunc(healthzPath, app.handleHealthzRequest)
	mux.Handle(ServePath, NewMaroonedPodsServerHandler(app.maroonedpodsNS, maroonedpodsCli))
	app.handler = cors.AllowAll().Handler(mux)

}

func (app *MaroonedPodsServer) handleHealthzRequest(w http.ResponseWriter, r *http.Request) {
	_, err := io.WriteString(w, "OK")
	if err != nil {
		klog.Errorf("handleHealthzRequest: failed to send response; %v", err)
	}
}

func (app *MaroonedPodsServer) Start() error {
	return app.startTLS()
}

func (app *MaroonedPodsServer) startTLS() error {
	var serveFunc func() error
	bindAddr := fmt.Sprintf("%s:%d", app.bindAddress, app.bindPort)
	tlsConfig := util.SetupTLS(app.secretCertManager)
	server := &http.Server{
		Addr:      bindAddr,
		Handler:   app.handler,
		TLSConfig: tlsConfig,
	}

	serveFunc = func() error {
		return server.ListenAndServeTLS("", "")
	}

	errChan := make(chan error)

	go func() {
		errChan <- serveFunc()
	}()
	// wait for server to exit
	return <-errChan
}
