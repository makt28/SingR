package libbox

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/locale"
	"github.com/sagernet/sing-box/log"
	poetShortcuts "github.com/sagernet/sing-box/poet/shortcuts"
	"github.com/sagernet/sing/common/byteformats"
)

var (
	sBasePath                string
	sWorkingPath             string
	sTempPath                string
	sUserID                  int
	sGroupID                 int
	sFixAndroidStack         bool
	sCommandServerListenPort uint16
	sCommandServerSecret     string
	sLogMaxLines             int
	sDebug                   bool
)

func init() {
	debug.SetPanicOnFault(true)
	debug.SetTraceback("all")
}

type SetupOptions struct {
	BasePath                string
	WorkingPath             string
	TempPath                string
	FixAndroidStack         bool
	CommandServerListenPort int32
	CommandServerSecret     string
	LogMaxLines             int
	Debug                   bool
}

func Setup(options *SetupOptions) error {
	sBasePath = options.BasePath
	sWorkingPath = options.WorkingPath
	sTempPath = options.TempPath

	sUserID = os.Getuid()
	sGroupID = os.Getgid()

	// TODO: remove after fixed
	// https://github.com/golang/go/issues/68760
	sFixAndroidStack = options.FixAndroidStack

	sCommandServerListenPort = uint16(options.CommandServerListenPort)
	sCommandServerSecret = options.CommandServerSecret
	sLogMaxLines = options.LogMaxLines
	sDebug = options.Debug

	os.MkdirAll(sWorkingPath, 0o777)
	os.MkdirAll(sTempPath, 0o777)

	// SingR/Android: the CLI feeds poet its panel config via `-p <file>`
	// (cmd/sing-box/cmd.go), but the libbox path has no such flag — so
	// box.Start()→POET.Start() would find an empty poetConfigPath and exit(1).
	// Point it at <workingPath>/panel.json, which the app writes before start.
	// This lives in libbox only; cmd/sing-box does not import it, so the Linux
	// binary and its `-p` behaviour are untouched.
	poetShortcuts.SetObject("poetConfigPath", filepath.Join(sWorkingPath, "panel.json"))
	return nil
}

func SetLocale(localeId string) {
	locale.Set(localeId)
}

func Version() string {
	return C.Version
}

func FormatBytes(length int64) string {
	return byteformats.FormatKBytes(uint64(length))
}

func FormatMemoryBytes(length int64) string {
	return byteformats.FormatMemoryKBytes(uint64(length))
}

func FormatDuration(duration int64) string {
	return log.FormatDuration(time.Duration(duration) * time.Millisecond)
}

func ProxyDisplayType(proxyType string) string {
	return C.ProxyDisplayName(proxyType)
}
