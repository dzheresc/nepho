package nepho

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// Labels put on services in addition to the common ones.
const (
	LKind    = "nepho-kind"    // postgres | valkey
	LVersion = "nepho-version" // major version
	LSecret  = "nepho-secret"  // on Docker: the generated password
)

// ServiceKind describes one kind of service.
type ServiceKind struct {
	Port     int
	Version  string // default version
	Scheme   string // URL scheme
	User     string
	Database string
}

var Services = map[string]ServiceKind{
	"postgres": {5432, "17", "postgresql", "nepho", "nepho"},
	"valkey":   {6379, "8", "redis", "default", "0"},
}

type ServiceSpec struct {
	Kind     string // postgres | valkey
	Version  string // major version; default: see Services
	CPUs     int    // default 1
	MemoryGB int    // default 1
	DiskGB   int    // default 20
	Labels   map[string]string
	TTL      time.Duration // default 4 hours; negative = never expires
}

// ServiceInfo is what a driver knows about one service. Host/Port is the
// endpoint as seen from the caller; InternalHost/InternalPort is the one to use
// from machines of the same provider. Either can be empty.
type ServiceInfo struct {
	ID           string
	Kind         string
	Version      string
	State        State
	Labels       map[string]string
	User         string
	Password     string
	Database     string
	Host         string
	Port         int
	InternalHost string
	InternalPort int
}

// Service is a running database or cache. Clients connect with their normal driver.
type Service struct {
	ServiceInfo
	Provider string
	Expires  time.Time // zero = never

	owner *Provider
}

func (s *Service) url(host string, port int) string {
	if host == "" {
		return ""
	}
	return fmt.Sprintf("%s://%s:%s@%s:%d/%s", Services[s.Kind].Scheme, s.User, s.Password, host, port, s.Database)
}

// URL is the connection string for the caller, or "" when not reachable from here.
func (s *Service) URL() string { return s.url(s.Host, s.Port) }

// InternalURL is the connection string for machines of the same provider, or "".
func (s *Service) InternalURL() string { return s.url(s.InternalHost, s.InternalPort) }

func (s *Service) Destroy(ctx context.Context) error {
	if err := s.owner.Destroy(ctx, s.ID); err != nil {
		return err
	}
	s.State = Terminated
	return nil
}

// ServiceDriver is implemented by backends that offer services.
type ServiceDriver interface {
	// LaunchService returns when the service accepts connections.
	LaunchService(ctx context.Context, id string, spec ServiceSpec, labels map[string]string) (ServiceInfo, error)
	ListServices(ctx context.Context) ([]ServiceInfo, error)
}

func (p *Provider) service(i ServiceInfo) *Service {
	return &Service{ServiceInfo: i, Provider: p.Name(), Expires: ExpiresOf(i.Labels), owner: p}
}

func (p *Provider) serviceDriver() (ServiceDriver, error) {
	if d, ok := p.d.(ServiceDriver); ok {
		return d, nil
	}
	return nil, fmt.Errorf("%w: %s: services are not supported yet", ErrProvision, p.Name())
}

// CreateService creates a database or cache and returns when it accepts connections.
func (p *Provider) CreateService(ctx context.Context, spec ServiceSpec) (*Service, error) {
	d, err := p.serviceDriver()
	if err != nil {
		return nil, err
	}
	kind, ok := Services[spec.Kind]
	if !ok {
		return nil, fmt.Errorf("%w: %s: unknown service kind %q", ErrProvision, p.Name(), spec.Kind)
	}
	if spec.Version == "" {
		spec.Version = kind.Version
	}
	if spec.CPUs == 0 {
		spec.CPUs = 1
	}
	if spec.MemoryGB == 0 {
		spec.MemoryGB = 1
	}
	if spec.DiskGB == 0 {
		spec.DiskGB = 20
	}
	if spec.TTL == 0 {
		spec.TTL = 4 * time.Hour
	}
	id := newID("s")
	labels := makeLabels(id, spec.Labels, spec.TTL)
	labels[LKind], labels[LVersion] = spec.Kind, spec.Version
	info, err := d.LaunchService(ctx, id, spec, labels)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
		defer cancel()
		_ = p.d.Destroy(cleanup, id)
		return nil, p.wrap(err)
	}
	return p.service(info), nil
}

// ListServices returns every service the backend still knows, in any state.
func (p *Provider) ListServices(ctx context.Context) ([]*Service, error) {
	d, ok := p.d.(ServiceDriver)
	if !ok {
		return nil, nil
	}
	infos, err := d.ListServices(ctx)
	if err != nil {
		return nil, p.wrap(err)
	}
	out := make([]*Service, len(infos))
	for i, info := range infos {
		out[i] = p.service(info)
	}
	return out, nil
}

func (p *Provider) AttachService(ctx context.Context, id string) (*Service, error) {
	all, err := p.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w: %s: no service %s", ErrProvision, p.Name(), id)
}

func makeLabels(id string, extra map[string]string, ttl time.Duration) map[string]string {
	labels := map[string]string{}
	for k, v := range extra {
		labels[k] = v
	}
	labels[LManaged], labels[LID] = "true", id
	if ttl > 0 {
		labels[LExpires] = strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	}
	return labels
}
