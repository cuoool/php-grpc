package grpc

import (
	"fmt"
	"github.com/spiral/php-grpc/parser"
	"github.com/spiral/roadrunner"
	"github.com/spiral/roadrunner/service/env"
	"github.com/spiral/roadrunner/service/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding"
	"path"
	"sync"
)

// ID sets public GRPC service ID for roadrunner.Container.
const ID = "grpc"

// Service manages set of GPRC services, options and connections.
type Service struct {
	cfg      *Config
	env      env.Environment
	list     []func(event int, ctx interface{})
	opts     []grpc.ServerOption
	services []func(server *grpc.Server)
	mu       sync.Mutex
	rr       *roadrunner.Server
	cr       roadrunner.Controller
	grpc     *grpc.Server
}

// Attach attaches cr. Currently only one cr is supported.
func (svc *Service) Attach(ctr roadrunner.Controller) {
	svc.cr = ctr
}

// AddListener attaches grpc event watcher.
func (svc *Service) AddListener(l func(event int, ctx interface{})) {
	svc.list = append(svc.list, l)
}

// AddService would be invoked after GRPC service creation.
func (svc *Service) AddService(r func(server *grpc.Server)) error {
	svc.services = append(svc.services, r)
	return nil
}

// AddOption adds new GRPC server option. Codec and TLS options are controlled by service internally.
func (svc *Service) AddOption(opt grpc.ServerOption) {
	svc.opts = append(svc.opts, opt)
}

// Init service.
func (svc *Service) Init(cfg *Config, r *rpc.Service, e env.Environment) (ok bool, err error) {
	svc.cfg = cfg
	svc.env = e

	if r != nil {
		if err := r.Register(ID, &rpcServer{svc}); err != nil {
			return false, err
		}
	}

	return true, nil
}

// Serve GRPC grpc.
func (svc *Service) Serve() (err error) {
	svc.mu.Lock()

	if svc.env != nil {
		if err := svc.env.Copy(svc.cfg.Workers); err != nil {
			return err
		}
	}

	svc.cfg.Workers.SetEnv("RR_GRPC", "true")

	svc.rr = roadrunner.NewServer(svc.cfg.Workers)
	svc.rr.Listen(svc.throw)

	if svc.cr != nil {
		svc.rr.Attach(svc.cr)
	}

	if svc.grpc, err = svc.createGPRCServer(); err != nil {
		return err
	}

	lis, err := svc.cfg.Listener()
	if err != nil {
		return err
	}

	defer lis.Close()

	svc.mu.Unlock()

	if err := svc.rr.Start(); err != nil {
		return err
	}
	defer svc.rr.Stop()

	return svc.grpc.Serve(lis)
}

// Stop the service.
func (svc *Service) Stop() {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.grpc == nil {
		return
	}

	go svc.grpc.GracefulStop()
}

// throw handles service, grpc and pool events.
func (svc *Service) throw(event int, ctx interface{}) {
	for _, l := range svc.list {
		l(event, ctx)
	}

	if event == roadrunner.EventServerFailure {
		// underlying rr grpc is dead
		svc.Stop()
	}
}

// new configured GRPC server
func (svc *Service) createGPRCServer() (*grpc.Server, error) {
	opts, err := svc.serverOptions()
	if err != nil {
		return nil, err
	}

	server := grpc.NewServer(opts...)

	// php proxy services
	services, err := parser.File(svc.cfg.Proto, path.Dir(svc.cfg.Proto))
	if err != nil {
		return nil, err
	}

	for _, service := range services {
		p := NewProxy(fmt.Sprintf("%s.%s", service.Package, service.Name), svc.cfg.Proto, svc.rr)
		for _, m := range service.Methods {
			p.RegisterMethod(m.Name)
		}

		server.RegisterService(p.ServiceDesc(), p)
	}

	// external services
	for _, r := range svc.services {
		r(server)
	}

	return server, nil
}

// server options
func (svc *Service) serverOptions() (opts []grpc.ServerOption, err error) {
	if svc.cfg.EnableTLS() {
		creds, err := credentials.NewServerTLSFromFile(svc.cfg.TLS.Cert, svc.cfg.TLS.Key)
		if err != nil {
			return nil, err
		}

		opts = append(opts, grpc.Creds(creds))
	}

	opts = append(opts, svc.opts...)

	// custom codec is required to bypass protobuf
	return append(opts, grpc.CustomCodec(&codec{encoding.GetCodec("proto")})), nil
}
