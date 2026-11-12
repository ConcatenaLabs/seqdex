package grpc_interface

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	pb "github.com/aejkcs50/seqdex/wallet/api-spec/protobuf/gen/go/ocean/v1"
	appconfig "github.com/aejkcs50/seqdex/wallet/internal/app-config"
	grpc_handler "github.com/aejkcs50/seqdex/wallet/internal/interfaces/grpc/handler"
	grpc_interceptor "github.com/aejkcs50/seqdex/wallet/internal/interfaces/grpc/interceptor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	tlsKeyFile        = "key.pem"
	tlsCertFile       = "cert.pem"
	serialNumberLimit = new(big.Int).Lsh(big.NewInt(1), 128)
)

type service struct {
	config                   ServiceConfig
	appConfig                *appconfig.AppConfig
	grpcServer               *grpc.Server
	chCloseStreamConnections chan (struct{})
	closeStreams             *sync.Once

	log  func(format string, a ...interface{})
	warn func(err error, format string, a ...interface{})
}

func NewService(config ServiceConfig, appConfig *appconfig.AppConfig) (*service, error) {
	logFn := func(format string, a ...interface{}) {
		format = fmt.Sprintf("service: %s", format)
		log.Infof(format, a...)
	}
	warnFn := func(err error, format string, a ...interface{}) {
		format = fmt.Sprintf("account service: %s", format)
		log.WithError(err).Warnf(format, a...)
	}
	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %s", err)
	}
	if err := appConfig.Validate(); err != nil {
		return nil, fmt.Errorf("invalid app config: %s", err)
	}

	if !config.insecure() {
		if err := generateTLSKeyPair(
			config.TLSLocation, config.ExtraIPs, config.ExtraDomains,
		); err != nil {
			return nil, fmt.Errorf("error while creating TLS keypair: %s", err)
		}
		logFn("created TLS keypair in path %s", config.TLSLocation)
	}
	chCloseStreamConnections := make(chan struct{})
	return &service{
		config, appConfig, nil, chCloseStreamConnections, &sync.Once{}, logFn, warnFn,
	}, nil
}

func (s *service) Start() error {
	s.appConfig.BlockchainScanner().Start()
	s.log("started blockchain scanner")

	srv, err := s.start()
	if err != nil {
		return err
	}

	s.log("start listening on %s", s.config.address())

	s.grpcServer = srv
	return nil
}

func (s *service) Stop() {
	onlyGrpcServer := true
	allServices := !onlyGrpcServer
	s.stop(allServices)
	s.log("shutdown")
}

func (s *service) start() (*grpc.Server, error) {
	grpcConfig := []grpc.ServerOption{
		grpc_interceptor.UnaryInterceptor(), grpc_interceptor.StreamInterceptor(),
	}
	if !s.config.insecure() {
		creds, err := credentials.NewServerTLSFromFile(
			s.config.tlsCertPath(), s.config.tlsKeyPath(),
		)
		if err != nil {
			return nil, err
		}
		grpcConfig = append(grpcConfig, grpc.Creds(creds))
	}

	grpcServer := grpc.NewServer(grpcConfig...)

	walletHandler := grpc_handler.NewWalletHandler(s.appConfig.WalletService())
	pb.RegisterWalletServiceServer(grpcServer, walletHandler)

	s.log("registered wallet handler on public interface")

	accountHandler := grpc_handler.NewAccountHandler(s.appConfig.AccountService())
	txHandler := grpc_handler.NewTransactionHandler(s.appConfig.TransactionService())
	notifyHandler := grpc_handler.NewNotificationHandler(
		s.appConfig.NotificationService(), s.chCloseStreamConnections,
	)

	pb.RegisterAccountServiceServer(grpcServer, accountHandler)
	pb.RegisterTransactionServiceServer(grpcServer, txHandler)
	pb.RegisterNotificationServiceServer(grpcServer, notifyHandler)
	s.log("registered account handler on public interface")
	s.log("registered transaction handler on public interface")
	s.log("registered notification handler on public interface")

	go grpcServer.Serve(s.config.listener())

	switch {
	case s.appConfig.WithAutoInit():
		go s.autoInitAndUnlock()
	case s.appConfig.WithAutoUnlock():
		go s.autoUnlock()
	}

	return grpcServer, nil
}

// gracefulStopTimeout is how long a stop waits for calls in progress to end before it cuts them.
const gracefulStopTimeout = 10 * time.Second

// stop ends every notification stream (closing the channel reaches every stream at once; a
// single send reached one, and GracefulStop then waited for the others, which clients keep
// open for good, until systemd killed the process), lets calls in progress end within
// gracefulStopTimeout, then saves where each account's scan has got to and closes the store.
func (s *service) stop(onlyGrpcServer bool) {
	s.closeStreams.Do(func() {
		close(s.chCloseStreamConnections)
		s.log("closed stream connections")
	})

	done := make(chan struct{})
	go func() {
		s.grpcServer.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		s.log("stopped grpc server")
	case <-time.After(gracefulStopTimeout):
		s.grpcServer.Stop()
		s.log("stopped grpc server (calls still running after %s were cut)", gracefulStopTimeout)
	}
	if onlyGrpcServer {
		return
	}

	s.appConfig.BlockchainScanner().Stop()
	s.log("stopped blockchain scanner")
	s.appConfig.RepoManager().Close()
	s.log("closed connection with db")
}

func (s *service) autoInitAndUnlock() {
	wallet := s.appConfig.WalletService()
	status := wallet.GetStatus(context.Background())
	if !status.IsInitialized {
		s.autoInit()
	}

	s.autoUnlock()
}

func (s *service) autoUnlock() {
	attempts := 0
	ctx := context.Background()
	wallet := s.appConfig.WalletService()
	for attempts < 3 {
		if err := wallet.Unlock(ctx, s.appConfig.Password); err != nil {
			attempts++
			s.warn(err, "failed to auto unlock, retrying...")
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.log("wallet auto unlocked")
		return
	}
	s.warn(nil, "failed to auto unlock, the operation must be done manually")
}

func (s *service) autoInit() {
	attempts := 0
	ctx := context.Background()
	wallet := s.appConfig.WalletService()
	for attempts < 3 {
		mnemonic := strings.Split(s.appConfig.Mnemonic, " ")
		if err := wallet.CreateWallet(
			ctx, mnemonic, s.appConfig.Password,
		); err != nil {
			attempts++
			s.warn(err, "failed to auto init, retrying...")
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.log("wallet auto initialized")
		return
	}
	s.warn(nil, "failed to auto initialize, the operation must be done manually")
}
