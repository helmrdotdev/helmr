//go:build linux

package datapath

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const helmrFilterPriority = uint16(1)

type InterfaceFacts struct {
	TapName     string
	GuestIPv4   netip.Addr
	GatewayIPv4 netip.Addr
	GuestMAC    net.HardwareAddr
}

type Authority struct {
	WorkerEpoch int64
	OwnerID     string
	Generation  int64
}

type BindingIdentity struct {
	Ifindex      int
	ProgramID    int
	ProgramTag   string
	FilterHandle uint32
	Mark         uint32
}

// DetachExact removes the persisted ingress classifier and its clsact qdisc
// only when the current interface and filter still exact-match the owner
// manifest. An already absent interface or classifier is a successful absence
// proof; replacements are never modified.
func DetachExact(tapName string, identity BindingIdentity) error {
	if strings.TrimSpace(tapName) == "" || identity.Ifindex <= 0 || identity.ProgramID <= 0 ||
		identity.ProgramTag == "" || identity.FilterHandle == 0 {
		return errors.New("persisted datapath identity is incomplete")
	}
	tap, err := netlink.LinkByName(tapName)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return fmt.Errorf("find exact TAP during persisted detach: %w", err)
	}
	if tap.Attrs().Index != identity.Ifindex {
		return errors.New("refusing to detach classifier from replacement TAP")
	}
	filters, err := netlink.FilterList(tap, netlink.HANDLE_MIN_INGRESS)
	if err != nil {
		return fmt.Errorf("list persisted TAP ingress filters: %w", err)
	}
	if len(filters) > 1 {
		return fmt.Errorf("interface %q has %d ingress filters, want at most one", tapName, len(filters))
	}
	clsact, exists, err := exactClsact(tap)
	if err != nil {
		return fmt.Errorf("verify persisted clsact on %q: %w", tapName, err)
	}
	if len(filters) == 1 {
		if !exists {
			return fmt.Errorf("interface %q has an ingress classifier without the owned clsact qdisc", tapName)
		}
		filter, ok := filters[0].(*netlink.BpfFilter)
		if !ok || filter.Handle != identity.FilterHandle || filter.Priority != helmrFilterPriority ||
			!filter.DirectAction || filter.Name != "helmr_ingress" ||
			filter.Id != identity.ProgramID || filter.Tag != identity.ProgramTag {
			return fmt.Errorf("interface %q ingress classifier does not match persisted identity", tapName)
		}
		if err := netlink.FilterDel(filter); err != nil {
			return fmt.Errorf("detach persisted ingress classifier from %q: %w", tapName, err)
		}
	}
	remaining, err := netlink.FilterList(tap, netlink.HANDLE_MIN_INGRESS)
	if err != nil {
		return fmt.Errorf("prove persisted ingress classifier absence on %q: %w", tapName, err)
	}
	if len(remaining) != 0 {
		return fmt.Errorf("interface %q retains %d ingress filters after persisted detach", tapName, len(remaining))
	}
	if exists {
		if err := netlink.QdiscDel(clsact); err != nil {
			return fmt.Errorf("remove persisted clsact from %q: %w", tapName, err)
		}
	}
	if _, exists, err := exactClsact(tap); err != nil {
		return fmt.Errorf("prove persisted clsact absence on %q: %w", tapName, err)
	} else if exists {
		return fmt.Errorf("interface %q retains clsact after persisted detach", tapName)
	}
	return nil
}

type Manager struct {
	mu       sync.Mutex
	marks    map[uint32]struct{}
	bindings map[*Binding]error
	verified bool
}

func NewManager() *Manager {
	return &Manager{
		marks:    make(map[uint32]struct{}),
		bindings: make(map[*Binding]error),
	}
}

type Binding struct {
	manager    *Manager
	objects    ingressObjects
	state      ingressBindingState
	authority  Authority
	facts      InterfaceFacts
	ifindex    int
	programID  int
	programTag string
	qdisc      *netlink.Clsact
	filter     *netlink.BpfFilter
	mark       uint32
	mu         sync.Mutex
	active     bool
	closed     bool
}

func (manager *Manager) VerifyKernel() error {
	if manager == nil {
		return errors.New("datapath manager is nil")
	}
	var objects ingressObjects
	if err := loadIngressObjects(&objects, nil); err != nil {
		return fmt.Errorf("verify ingress classifier load: %w", err)
	}
	if err := objects.Close(); err != nil {
		return fmt.Errorf("close ingress classifier proof: %w", err)
	}
	manager.mu.Lock()
	manager.verified = true
	manager.mu.Unlock()
	return nil
}

func (manager *Manager) Health() error {
	if manager == nil {
		return errors.New("datapath manager is nil")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !manager.verified {
		return errors.New("datapath startup proof is incomplete")
	}
	for _, err := range manager.bindings {
		if err != nil {
			return fmt.Errorf("datapath binding is invalid: %w", err)
		}
	}
	return nil
}

func (manager *Manager) Prepare(authority Authority, facts InterfaceFacts) (_ *Binding, returnErr error) {
	if manager == nil {
		return nil, errors.New("datapath manager is nil")
	}
	if err := validateAuthority(authority); err != nil {
		return nil, err
	}
	if err := validateInterfaceFacts(facts); err != nil {
		return nil, err
	}
	if err := manager.Health(); err != nil {
		return nil, err
	}
	tap, err := netlink.LinkByName(facts.TapName)
	if err != nil {
		return nil, fmt.Errorf("find exact TAP %q: %w", facts.TapName, err)
	}
	if tap.Attrs().Index <= 0 {
		return nil, errors.New("datapath TAP does not have a positive ifindex")
	}
	filters, err := netlink.FilterList(tap, netlink.HANDLE_MIN_INGRESS)
	if err != nil {
		return nil, fmt.Errorf("list TAP ingress filters: %w", err)
	}
	if len(filters) != 0 {
		return nil, errors.New("datapath TAP ingress is not empty before binding")
	}

	mark, err := manager.allocateMark()
	if err != nil {
		return nil, err
	}
	binding := &Binding{
		manager:   manager,
		mark:      mark,
		authority: authority,
		facts: InterfaceFacts{
			TapName: facts.TapName, GuestIPv4: facts.GuestIPv4,
			GatewayIPv4: facts.GatewayIPv4,
			GuestMAC:    append(net.HardwareAddr(nil), facts.GuestMAC...),
		},
		ifindex: tap.Attrs().Index,
	}
	defer func() {
		if returnErr != nil {
			_ = binding.Close()
		}
	}()
	if _, exists, err := exactClsact(tap); err != nil {
		return nil, fmt.Errorf("verify TAP clsact absence before binding: %w", err)
	} else if exists {
		return nil, errors.New("datapath TAP clsact is not empty before binding")
	}
	binding.qdisc = newClsact(tap)
	if err := netlink.QdiscAdd(binding.qdisc); err != nil {
		binding.qdisc = nil
		return nil, fmt.Errorf("add clsact to %q: %w", facts.TapName, err)
	}
	if err := loadIngressObjects(&binding.objects, nil); err != nil {
		return nil, fmt.Errorf("load ingress classifier: %w", err)
	}
	guestIPv4 := facts.GuestIPv4.As4()
	gatewayIPv4 := facts.GatewayIPv4.As4()
	var guestMAC [6]uint8
	copy(guestMAC[:], facts.GuestMAC)
	binding.state = ingressBindingState{
		Mark:        mark,
		GuestIpv4:   guestIPv4,
		GatewayIpv4: gatewayIPv4,
		GuestMac:    guestMAC,
	}
	var stateKey uint32
	if err := binding.objects.State.Put(stateKey, binding.state); err != nil {
		return nil, fmt.Errorf("populate inactive ingress binding state: %w", err)
	}
	info, err := binding.objects.HelmrIngress.Info()
	if err != nil {
		return nil, fmt.Errorf("inspect ingress classifier identity: %w", err)
	}
	programID, ok := info.ID()
	if !ok || programID == 0 || info.Tag == "" {
		return nil, errors.New("ingress classifier has no stable kernel identity")
	}
	binding.programID = int(programID)
	binding.programTag = info.Tag
	filter, err := attachClassifier(tap, binding.objects.HelmrIngress)
	if err != nil {
		return nil, err
	}
	binding.filter = filter
	if err := verifyClassifier(tap, binding.programID, binding.programTag); err != nil {
		return nil, err
	}
	manager.mu.Lock()
	manager.bindings[binding] = nil
	manager.mu.Unlock()
	return binding, nil
}

func (binding *Binding) Verify() error {
	if binding == nil {
		return errors.New("datapath binding is nil")
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if binding.closed {
		return errors.New("datapath binding is closed")
	}
	tap, err := netlink.LinkByName(binding.facts.TapName)
	if err != nil {
		return fmt.Errorf("find exact TAP during rescan: %w", err)
	}
	if tap.Attrs().Index != binding.ifindex {
		return errors.New("exact TAP ifindex changed")
	}
	if err := verifyClassifier(tap, binding.programID, binding.programTag); err != nil {
		return err
	}
	var stateKey uint32
	var actualState ingressBindingState
	if err := binding.objects.State.Lookup(stateKey, &actualState); err != nil {
		return fmt.Errorf("verify ingress binding state: %w", err)
	}
	if actualState != binding.state {
		return errors.New("ingress binding state changed")
	}
	return nil
}

func (binding *Binding) Invalidate(cause error) error {
	if binding == nil {
		return nil
	}
	if cause == nil {
		cause = errors.New("local datapath changed")
	}
	binding.mu.Lock()
	if binding.closed {
		binding.mu.Unlock()
		return nil
	}
	deactivateErr := binding.setActive(0)
	binding.active = false
	binding.mu.Unlock()
	binding.manager.mu.Lock()
	binding.manager.bindings[binding] = errors.Join(cause, deactivateErr)
	binding.manager.mu.Unlock()
	return deactivateErr
}

func (binding *Binding) Mark() uint32 {
	if binding == nil {
		return 0
	}
	return binding.mark
}

func (binding *Binding) Identity() (BindingIdentity, error) {
	if binding == nil {
		return BindingIdentity{}, errors.New("datapath binding is nil")
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if binding.closed || binding.filter == nil {
		return BindingIdentity{}, errors.New("datapath binding is not installed")
	}
	return BindingIdentity{
		Ifindex:      binding.ifindex,
		ProgramID:    binding.programID,
		ProgramTag:   binding.programTag,
		FilterHandle: binding.filter.Handle,
		Mark:         binding.mark,
	}, nil
}

func (binding *Binding) Activate() error {
	if binding == nil {
		return errors.New("datapath binding is nil")
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if binding.closed {
		return errors.New("datapath binding is closed")
	}
	if binding.active {
		return errors.New("datapath binding is already active")
	}
	if err := binding.setActive(1); err != nil {
		_ = binding.setActive(0)
		return err
	}
	binding.active = true
	return nil
}

func (binding *Binding) Deactivate() error {
	if binding == nil {
		return nil
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if binding.closed || !binding.active {
		return nil
	}
	if err := binding.setActive(0); err != nil {
		return err
	}
	binding.active = false
	return nil
}

func (binding *Binding) Close() error {
	if binding == nil {
		return nil
	}
	binding.mu.Lock()
	defer binding.mu.Unlock()
	if binding.closed {
		return nil
	}
	var deactivateErr error
	if binding.active {
		deactivateErr = binding.setActive(0)
		if deactivateErr == nil {
			binding.active = false
		}
	}
	detachErr := binding.detachClassifier()
	if detachErr != nil {
		return errors.Join(deactivateErr, detachErr)
	}
	closeErr := binding.objects.Close()
	binding.closed = true
	binding.manager.release(binding)
	return errors.Join(deactivateErr, closeErr)
}

func (binding *Binding) setActive(active uint32) error {
	state := binding.state
	state.Active = active
	var key uint32
	if err := binding.objects.State.Put(key, state); err != nil {
		return fmt.Errorf("set ingress binding active=%d: %w", active, err)
	}
	binding.state = state
	return nil
}

func validateInterfaceFacts(facts InterfaceFacts) error {
	if strings.TrimSpace(facts.TapName) == "" {
		return errors.New("datapath TAP name is required")
	}
	if !facts.GuestIPv4.Is4() || !facts.GatewayIPv4.Is4() ||
		facts.GuestIPv4.IsUnspecified() || facts.GatewayIPv4.IsUnspecified() ||
		facts.GuestIPv4 == facts.GatewayIPv4 {
		return errors.New("datapath guest and gateway addresses must be specified IPv4 addresses")
	}
	if len(facts.GuestMAC) != 6 || facts.GuestMAC[0]&1 != 0 || bytesZero(facts.GuestMAC) {
		return errors.New("datapath guest MAC must be six bytes")
	}
	return nil
}

func validateAuthority(authority Authority) error {
	if authority.WorkerEpoch <= 0 || authority.Generation <= 0 || strings.TrimSpace(authority.OwnerID) == "" {
		return errors.New("datapath authority is incomplete")
	}
	return nil
}

func bytesZero(value []byte) bool {
	for _, current := range value {
		if current != 0 {
			return false
		}
	}
	return true
}

func attachClassifier(link netlink.Link, program *ebpf.Program) (*netlink.BpfFilter, error) {
	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Handle:    netlink.MakeHandle(0, 1),
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Priority:  helmrFilterPriority,
			Protocol:  unix.ETH_P_ALL,
		},
		Fd:           program.FD(),
		Name:         "helmr_ingress",
		DirectAction: true,
	}
	if err := netlink.FilterAdd(filter); err != nil {
		return nil, fmt.Errorf("attach ingress classifier to %q: %w", link.Attrs().Name, err)
	}
	return filter, nil
}

func (binding *Binding) detachClassifier() error {
	if binding.filter == nil && binding.qdisc == nil {
		return nil
	}
	tap, err := netlink.LinkByName(binding.facts.TapName)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			binding.filter = nil
			binding.qdisc = nil
			return nil
		}
		return fmt.Errorf("find exact TAP during detach: %w", err)
	}
	if tap.Attrs().Index != binding.ifindex {
		return errors.New("refusing to detach classifier from replacement TAP")
	}
	clsact, exists, err := exactClsact(tap)
	if err != nil {
		return fmt.Errorf("verify exact clsact before detach: %w", err)
	}
	if binding.qdisc != nil && !exists {
		return errors.New("owned clsact disappeared before detach")
	}
	if binding.filter != nil {
		if err := verifyClassifier(tap, binding.programID, binding.programTag); err != nil {
			return fmt.Errorf("verify exact classifier before detach: %w", err)
		}
		if err := netlink.FilterDel(binding.filter); err != nil {
			return fmt.Errorf("detach ingress classifier from %q: %w", tap.Attrs().Name, err)
		}
	}
	filters, err := netlink.FilterList(tap, netlink.HANDLE_MIN_INGRESS)
	if err != nil {
		return fmt.Errorf("verify ingress classifier absence on %q: %w", tap.Attrs().Name, err)
	}
	if len(filters) != 0 {
		return fmt.Errorf("interface %q retains %d ingress filters after detach", tap.Attrs().Name, len(filters))
	}
	binding.filter = nil
	if binding.qdisc != nil {
		if err := netlink.QdiscDel(clsact); err != nil {
			return fmt.Errorf("remove clsact from %q: %w", tap.Attrs().Name, err)
		}
	}
	if _, exists, err := exactClsact(tap); err != nil {
		return fmt.Errorf("verify clsact absence on %q: %w", tap.Attrs().Name, err)
	} else if exists {
		return fmt.Errorf("interface %q retains clsact after detach", tap.Attrs().Name)
	}
	binding.qdisc = nil
	return nil
}

func verifyClassifier(link netlink.Link, programID int, programTag string) error {
	if _, exists, err := exactClsact(link); err != nil {
		return fmt.Errorf("verify clsact on %q: %w", link.Attrs().Name, err)
	} else if !exists {
		return fmt.Errorf("interface %q does not have the owned clsact qdisc", link.Attrs().Name)
	}
	filters, err := netlink.FilterList(link, netlink.HANDLE_MIN_INGRESS)
	if err != nil {
		return fmt.Errorf("verify ingress filters on %q: %w", link.Attrs().Name, err)
	}
	if len(filters) != 1 {
		return fmt.Errorf("interface %q has %d ingress filters, want 1", link.Attrs().Name, len(filters))
	}
	filter, ok := filters[0].(*netlink.BpfFilter)
	if !ok || filter.Priority != helmrFilterPriority || !filter.DirectAction ||
		filter.Name != "helmr_ingress" || filter.Id != programID || filter.Tag != programTag {
		return fmt.Errorf("interface %q ingress classifier identity changed", link.Attrs().Name)
	}
	return nil
}

func newClsact(link netlink.Link) *netlink.Clsact {
	return &netlink.Clsact{QdiscAttrs: netlink.QdiscAttrs{
		LinkIndex: link.Attrs().Index,
		Handle:    netlink.MakeHandle(0xffff, 0),
		Parent:    netlink.HANDLE_CLSACT,
	}}
}

func exactClsact(link netlink.Link) (*netlink.Clsact, bool, error) {
	qdiscs, err := netlink.QdiscList(link)
	if err != nil {
		return nil, false, fmt.Errorf("list qdiscs: %w", err)
	}
	var found *netlink.Clsact
	for _, qdisc := range qdiscs {
		if qdisc.Type() != "clsact" {
			continue
		}
		current, ok := qdisc.(*netlink.Clsact)
		if !ok {
			return nil, false, errors.New("clsact qdisc has an unexpected netlink type")
		}
		attrs := current.Attrs()
		if attrs.LinkIndex != link.Attrs().Index || attrs.Handle != netlink.MakeHandle(0xffff, 0) ||
			attrs.Parent != netlink.HANDLE_CLSACT {
			return nil, false, errors.New("clsact qdisc identity changed")
		}
		if found != nil {
			return nil, false, errors.New("multiple clsact qdiscs are installed")
		}
		found = current
	}
	return found, found != nil, nil
}

func (manager *Manager) allocateMark() (uint32, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for attempt := 0; attempt < 32; attempt++ {
		var raw [4]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return 0, fmt.Errorf("generate binding mark: %w", err)
		}
		mark := binary.LittleEndian.Uint32(raw[:])
		if mark == 0 {
			continue
		}
		if _, exists := manager.marks[mark]; exists {
			continue
		}
		manager.marks[mark] = struct{}{}
		return mark, nil
	}
	return 0, errors.New("could not allocate a unique binding mark")
}

func (manager *Manager) release(binding *Binding) {
	manager.mu.Lock()
	delete(manager.marks, binding.mark)
	delete(manager.bindings, binding)
	manager.mu.Unlock()
}
