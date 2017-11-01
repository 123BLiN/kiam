// Copyright 2017 uSwitch
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package metadata

import (
	"context"
	"fmt"
	"github.com/gorilla/mux"
	"github.com/rcrowley/go-metrics"
	"github.com/rcrowley/go-metrics/exp"
	log "github.com/sirupsen/logrus"
	"github.com/uswitch/kiam/pkg/aws/sts"
	khttp "github.com/uswitch/kiam/pkg/http"
	"github.com/uswitch/kiam/pkg/k8s"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

type Server struct {
	cfg    *ServerConfig
	server *http.Server
}

type ServerConfig struct {
	ListenPort       int
	MetadataEndpoint string
	AllowIPQuery     bool
	MaxElapsedTime   time.Duration
}

func NewConfig(port int) *ServerConfig {
	return &ServerConfig{
		MetadataEndpoint: "http://169.254.169.254",
		ListenPort:       port,
		AllowIPQuery:     false,
		MaxElapsedTime:   time.Second * 10,
	}
}

func NewWebServer(config *ServerConfig, finder k8s.RoleFinder, credentials sts.CredentialsProvider) (*Server, error) {
	http, err := buildHTTPServer(config, finder, credentials)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: config, server: http}, nil
}

func buildClientIP(config *ServerConfig) func(*http.Request) (string, error) {
	remote := func(req *http.Request) (string, error) {
		return ParseClientIP(req.RemoteAddr)
	}

	if config.AllowIPQuery {
		return func(req *http.Request) (string, error) {
			ip := req.Form.Get("ip")
			if ip != "" {
				return ip, nil
			}
			return remote(req)
		}
	}

	return remote
}

func buildHTTPServer(config *ServerConfig, finder k8s.RoleFinder, credentials sts.CredentialsProvider) (*http.Server, error) {
	router := mux.NewRouter()
	router.Handle("/metrics", exp.ExpHandler(metrics.DefaultRegistry))
	router.Handle("/ping", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "pong") }))

	h := &healthHandler{config.MetadataEndpoint}
	router.Handle("/health", http.HandlerFunc(errorHandler("health", h)))

	clientIP := buildClientIP(config)

	r := &roleHandler{
		roleFinder: finder,
		clientIP:   clientIP,
	}
	router.Handle("/{version}/meta-data/iam/security-credentials/", http.HandlerFunc(errorHandler("roleName", r)))

	c := &credentialsHandler{
		roleFinder:          finder,
		credentialsProvider: credentials,
		clientIP:            clientIP,
	}
	router.Handle("/{version}/meta-data/iam/security-credentials/{role:.*}", http.HandlerFunc(errorHandler("credentials", c)))

	metadataURL, err := url.Parse(config.MetadataEndpoint)
	if err != nil {
		return nil, err
	}
	router.Handle("/{path:.*}", httputil.NewSingleHostReverseProxy(metadataURL))

	listen := fmt.Sprintf(":%d", config.ListenPort)
	return &http.Server{Addr: listen, Handler: khttp.LoggingHandler(router)}, nil
}

func (s *Server) Serve() error {
	log.Infof("listening :%d", s.cfg.ListenPort)
	return s.server.ListenAndServe()
}

func (s *Server) Stop(ctx context.Context) {
	log.Infoln("starting server shutdown")
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s.server.Shutdown(c)
	log.Infoln("gracefully shutdown server")
}

func ParseClientIP(addr string) (string, error) {
	parts := strings.Split(addr, ":")
	if len(parts) < 2 {
		return "", fmt.Errorf("incorrect format, expected ip:port, was: %s", addr)
	}

	return strings.Join(parts[0:len(parts)-1], ":"), nil
}

func (s *Server) clientIP(req *http.Request) (string, error) {
	if s.cfg.AllowIPQuery {
	}

	return ParseClientIP(req.RemoteAddr)
}

func getStatusBucket(status int) string {
	if status >= 200 && status < 300 {
		return "2xx"
	}
	if status >= 300 && status < 400 {
		return "3xx"
	}
	if status >= 400 && status < 500 {
		return "4xx"
	}
	if status >= 500 && status < 600 {
		return "5xx"
	}
	return "unknown"
}

func getResponseMeter(name string, result int) metrics.Meter {
	bucket := getStatusBucket(result)
	return metrics.GetOrRegisterMeter(fmt.Sprintf("handlerResponse-%s.%s", name, bucket), metrics.DefaultRegistry)
}

const (
	handlerMaxDuration = time.Second * 5
)

func errorHandler(name string, handle handler) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), handlerMaxDuration)
		defer cancel()

		status, err := handle.Handle(ctx, w, req)
		getResponseMeter(name, status).Mark(1)

		if err != nil {
			log.WithFields(khttp.RequestFields(req)).WithField("status", status).Errorf("error processing request: %s", err.Error())
			http.Error(w, err.Error(), status)
		}
	}
}
