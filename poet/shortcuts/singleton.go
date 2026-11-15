package shortcuts

import (
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/poet/controller"
)

// singleton is the process-wide registry that connects sing-box (which
// creates inbounds and routes connections) with the panel layer (which owns
// one controller per panel node). It exists because the hooks in the
// upstream files are single function calls with no place to carry state.
type singleton struct {
	globalMap sync.Map

	Logger log.Logger

	// access guards the two maps below. They are written while a box is
	// being built / the panel is starting and read on every routed
	// connection; a config reload does both at once.
	access sync.RWMutex
	// inbounds: inbound tag → inbound, registered by the inbound manager.
	inbounds map[string]*adapter.Inbound
	// controllers: inbound tag → the controller of the panel node bound to it.
	controllers map[string]*controller.Controller
}

var (
	instance *singleton
	once     sync.Once
)

func Singleton() *singleton {
	once.Do(func() {
		instance = &singleton{
			inbounds:    make(map[string]*adapter.Inbound),
			controllers: make(map[string]*controller.Controller),
		}
	})
	return instance
}

func GetObject(k string) (interface{}, bool) {
	return Singleton().globalMap.Load(k)
}

func SetObject(k string, v interface{}) {
	Singleton().globalMap.Store(k, v)
}
