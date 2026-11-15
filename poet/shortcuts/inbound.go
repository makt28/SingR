package shortcuts

import (
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/poet/controller"
)

// SetInbound registers an inbound under its tag (called by the inbound
// manager for every inbound it creates).
func (ss *singleton) SetInbound(in *adapter.Inbound, tag string) {
	ss.access.Lock()
	defer ss.access.Unlock()
	ss.inbounds[tag] = in
}

// Inbound returns the inbound registered under tag, or nil.
func (ss *singleton) Inbound(tag string) *adapter.Inbound {
	ss.access.RLock()
	defer ss.access.RUnlock()
	return ss.inbounds[tag]
}

// RemoveInbound forgets the inbound registered under tag.
func (ss *singleton) RemoveInbound(tag string) {
	ss.access.Lock()
	defer ss.access.Unlock()
	delete(ss.inbounds, tag)
}

// ClearInbounds forgets every registered inbound.
func (ss *singleton) ClearInbounds() {
	ss.access.Lock()
	defer ss.access.Unlock()
	ss.inbounds = make(map[string]*adapter.Inbound)
}

// SetController binds a panel node's controller to the inbound tag its
// connections arrive on.
func (ss *singleton) SetController(ct *controller.Controller, inTag string) {
	ss.access.Lock()
	defer ss.access.Unlock()
	ss.controllers[inTag] = ct
}

// ControllerByInTag returns the controller bound to an inbound tag, or nil.
func (ss *singleton) ControllerByInTag(inTag string) *controller.Controller {
	ss.access.RLock()
	defer ss.access.RUnlock()
	return ss.controllers[inTag]
}

// RemoveController unbinds the controller of an inbound tag.
func (ss *singleton) RemoveController(inTag string) {
	ss.access.Lock()
	defer ss.access.Unlock()
	delete(ss.controllers, inTag)
}
