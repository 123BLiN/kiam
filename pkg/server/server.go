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
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/uswitch/k8sc/official"
	"github.com/uswitch/kiam/pkg/aws/sts"
	"github.com/uswitch/kiam/pkg/k8s"
	"github.com/uswitch/kiam/pkg/prefetch"
	pb "github.com/uswitch/kiam/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"io/ioutil"
	"net"
	"time"
)

type Config struct {
	BindAddress              string
	KubeConfig               string
	PodSyncInterval          time.Duration
	SessionName              string
	RoleBaseARN              string
	TLS                      *TLSConfig
	ParallelFetcherProcesses int
	PrefetchBufferSize       int
}

type TLSConfig struct {
	ServerCert string
	ServerKey  string
	CA         string
}

type KiamServer struct {
	listener            net.Listener
	server              *grpc.Server
	pods                *k8s.PodCache
	namespaces          *k8s.NamespaceCache
	manager             *prefetch.CredentialManager
	credentialsProvider sts.CredentialsProvider
	parallelFetchers    int
}

var (
	PodNotFoundError = fmt.Errorf("pod not found")
)

func (k *KiamServer) GetHealth(ctx context.Context, _ *pb.GetHealthRequest) (*pb.HealthStatus, error) {
	return &pb.HealthStatus{Message: "ok"}, nil
}

func (k *KiamServer) GetPodRole(ctx context.Context, req *pb.GetPodRoleRequest) (*pb.Role, error) {
	logger := log.WithField("pod.ip", req.Ip)
	pod, err := k.pods.FindPodForIP(req.Ip)
	if err != nil {
		logger.Errorf("error finding pod: %s", err.Error())
		return nil, err
	}

	if pod == nil {
		return nil, PodNotFoundError
	}

	role := k8s.PodRole(pod)

	logger.WithField("pod.iam.role", role).Infof("found role")

	return &pb.Role{Name: role}, nil
}

func translateCredentialsToProto(credentials *sts.Credentials) *pb.Credentials {
	return &pb.Credentials{
		Code:            credentials.Code,
		Type:            credentials.Type,
		AccessKeyId:     credentials.AccessKeyId,
		SecretAccessKey: credentials.SecretAccessKey,
		Token:           credentials.Token,
		Expiration:      credentials.Expiration,
		LastUpdated:     credentials.LastUpdated,
	}
}

func (k *KiamServer) GetRoleCredentials(ctx context.Context, req *pb.GetRoleCredentialsRequest) (*pb.Credentials, error) {
	logger := log.WithField("pod.iam.role", req.Role.Name)

	logger.Infof("requesting credentials")
	credentials, err := k.credentialsProvider.CredentialsForRole(ctx, req.Role.Name)
	if err != nil {
		logger.Errorf("error requesting credentials: %s", err.Error())
		return nil, err
	}

	return translateCredentialsToProto(credentials), nil
}

func NewServer(config *Config) (*KiamServer, error) {
	server := &KiamServer{parallelFetchers: config.ParallelFetcherProcesses}

	listener, err := net.Listen("tcp", config.BindAddress)
	if err != nil {
		return nil, err
	}
	server.listener = listener

	client, err := official.NewClient(config.KubeConfig)
	if err != nil {
		log.Fatalf("couldn't create kubernetes client: %s", err.Error())
	}
	server.pods = k8s.NewPodCache(k8s.KubernetesSource(client, k8s.ResourcePods), config.PodSyncInterval, config.PrefetchBufferSize)
	server.namespaces = k8s.NewNamespaceCache(k8s.KubernetesSource(client, k8s.ResourceNamespaces), time.Minute)

	stsGateway := sts.DefaultGateway()
	credentialsCache := sts.DefaultCache(stsGateway, config.RoleBaseARN, config.SessionName)
	server.credentialsProvider = credentialsCache
	server.manager = prefetch.NewManager(credentialsCache, server.pods, server.pods)

	certificate, err := tls.LoadX509KeyPair(config.TLS.ServerCert, config.TLS.ServerKey)
	if err != nil {
		return nil, err
	}
	certPool := x509.NewCertPool()
	if err != nil {
		return nil, err
	}
	ca, err := ioutil.ReadFile(config.TLS.CA)
	if err != nil {
		return nil, err
	}
	if ok := certPool.AppendCertsFromPEM(ca); !ok {
		return nil, fmt.Errorf("failed to append client certs")
	}
	creds := credentials.NewTLS(&tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{certificate},
		ClientCAs:    certPool,
	})

	grpcServer := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterKiamServiceServer(grpcServer, ServerWithTelemetry(server))
	server.server = grpcServer

	return server, nil
}

func (s *KiamServer) Serve(ctx context.Context) {
	s.pods.Run(ctx)
	s.namespaces.Run(ctx)
	s.manager.Run(ctx, s.parallelFetchers)
	s.server.Serve(s.listener)
}

func (s *KiamServer) Stop() {
	s.server.GracefulStop()
}
