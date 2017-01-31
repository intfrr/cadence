package membership

import (
	"sync"

	"github.com/dgryski/go-farm"
	"github.com/uber-common/bark"
	"github.com/uber/ringpop-go"
	"github.com/uber/ringpop-go/events"
	"github.com/uber/ringpop-go/hashring"
	"github.com/uber/ringpop-go/swim"
)

// RoleKey label is set by every single service as soon as it bootstraps its
// ringpop instance. The data for this key is the service name
const RoleKey = "serviceName"

type ringpopServiceResolver struct {
	service      string
	rp           *ringpop.Ringpop
	ring         *hashring.HashRing
	ringLock     sync.RWMutex
	listeners    map[string]chan<- *ChangedEvent
	listenerLock sync.RWMutex
	logger       bark.Logger
}

var _ ServiceResolver = (*ringpopServiceResolver)(nil)

func newRingpopServiceResolver(service string, rp *ringpop.Ringpop, logger bark.Logger) *ringpopServiceResolver {
	return &ringpopServiceResolver{
		service:   service,
		rp:        rp,
		logger:    logger.WithFields(bark.Fields{"component": "ServiceResolver", RoleKey: service}),
		ring:      hashring.New(farm.Fingerprint32, 1),
		listeners: make(map[string]chan<- *ChangedEvent),
	}
}

// Start starts the oracle
func (r *ringpopServiceResolver) Start() error {
	r.ringLock.Lock()
	defer r.ringLock.Unlock()

	r.rp.AddListener(r)
	addrs, err := r.rp.GetReachableMembers(swim.MemberWithLabelAndValue(RoleKey, r.service))
	if err != nil {
		return err
	}

	for _, addr := range addrs {
		labels := r.getLabelsMap()
		r.ring.AddMembers(NewHostInfo(addr, labels))
	}

	return nil
}

// Stop stops the oracle
func (r *ringpopServiceResolver) Stop() error {
	r.ringLock.Lock()
	r.listenerLock.Lock()
	defer r.listenerLock.Unlock()
	defer r.ringLock.Unlock()

	r.rp.RemoveListener(r)
	r.ring = hashring.New(farm.Fingerprint32, 1)
	r.listeners = make(map[string]chan<- *ChangedEvent)
	return nil
}

// Lookup finds the host in the ring responsible for serving the given key
func (r *ringpopServiceResolver) Lookup(key string) (*HostInfo, error) {
	r.ringLock.RLock()
	defer r.ringLock.RUnlock()
	addr, found := r.ring.Lookup(key)
	if !found {
		return nil, ErrInsufficientHosts
	}
	return NewHostInfo(addr, r.getLabelsMap()), nil
}

func (r *ringpopServiceResolver) AddListener(name string, notifyChannel chan<- *ChangedEvent) error {
	r.listenerLock.Lock()
	defer r.listenerLock.Unlock()
	_, ok := r.listeners[name]
	if ok {
		return ErrListenerAlreadyExist
	}
	r.listeners[name] = notifyChannel
	return nil
}

func (r *ringpopServiceResolver) RemoveListener(name string) error {
	r.listenerLock.Lock()
	defer r.listenerLock.Unlock()
	_, ok := r.listeners[name]
	if !ok {
		return nil
	}
	delete(r.listeners, name)
	return nil
}

// HandleEvent handles updates from ringpop
func (r *ringpopServiceResolver) HandleEvent(event events.Event) {
	// We only care about RingChangedEvent
	e, ok := event.(events.RingChangedEvent)
	if ok {
		r.logger.Info("Received a ring changed event")
		// Note that we receive events asynchronously, possibly out of order.
		// We cannot rely on the content of the event, rather we load everything
		// from ringpop when we get a notification that something changed.
		r.refresh()
		r.emitEvent(e)
	}
}

func (r *ringpopServiceResolver) refresh() {
	r.ringLock.Lock()
	defer r.ringLock.Unlock()

	r.ring = hashring.New(farm.Fingerprint32, 1)

	addrs, err := r.rp.GetReachableMembers(swim.MemberWithLabelAndValue(RoleKey, r.service))
	if err != nil {
		// This should never happen!
		r.logger.Panic(err)
	}

	for _, addr := range addrs {
		host := NewHostInfo(addr, r.getLabelsMap())
		r.ring.AddMembers(host)
	}

	r.logger.Infof("Current reachable members: %v", addrs)
}

func (r *ringpopServiceResolver) emitEvent(rpEvent events.RingChangedEvent) {
	// Marshall the event object into the required type
	event := &ChangedEvent{}
	for _, addr := range rpEvent.ServersAdded {
		event.HostsAdded = append(event.HostsAdded, NewHostInfo(addr, r.getLabelsMap()))
	}
	for _, addr := range rpEvent.ServersRemoved {
		event.HostsRemoved = append(event.HostsRemoved, NewHostInfo(addr, r.getLabelsMap()))
	}
	for _, addr := range rpEvent.ServersUpdated {
		event.HostsUpdated = append(event.HostsUpdated, NewHostInfo(addr, r.getLabelsMap()))
	}

	// Notify listeners
	r.listenerLock.RLock()
	defer r.listenerLock.RUnlock()

	for name, ch := range r.listeners {
		select {
		case ch <- event:
		default:
			r.logger.WithFields(bark.Fields{`listenerName`: name}).Error("Failed to send listener notification, channel full")
		}
	}
}

func (r *ringpopServiceResolver) getLabelsMap() map[string]string {
	labels := make(map[string]string)
	labels[RoleKey] = r.service
	return labels
}