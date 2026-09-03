package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	runtimeDebug "runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sagernet/sing-box"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	POET "github.com/sagernet/sing-box/poet"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badjson"

	"github.com/spf13/cobra"
)

var commandRun = &cobra.Command{
	Use:   "run",
	Short: "Run service",
	Run: func(cmd *cobra.Command, args []string) {
		err := run()
		if err != nil {
			log.Fatal(err)
		}
	},
}

func init() {
	mainCommand.AddCommand(commandRun)
}

type OptionsEntry struct {
	content []byte
	path    string
	options option.Options
}

func readConfigAt(path string) (*OptionsEntry, error) {
	var (
		configContent []byte
		err           error
	)
	if path == "stdin" {
		configContent, err = io.ReadAll(os.Stdin)
	} else {
		configContent, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, E.Cause(err, "read config at ", path)
	}
	options, err := json.UnmarshalExtendedContext[option.Options](globalCtx, configContent)
	if err != nil {
		return nil, E.Cause(err, "decode config at ", path)
	}
	return &OptionsEntry{
		content: configContent,
		path:    path,
		options: options,
	}, nil
}

func readConfig() ([]*OptionsEntry, error) {
	var optionsList []*OptionsEntry
	for _, path := range configPaths {
		optionsEntry, err := readConfigAt(path)
		if err != nil {
			return nil, err
		}
		optionsList = append(optionsList, optionsEntry)
	}
	for _, directory := range configDirectories {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil, E.Cause(err, "read config directory at ", directory)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") || entry.IsDir() {
				continue
			}
			optionsEntry, err := readConfigAt(filepath.Join(directory, entry.Name()))
			if err != nil {
				return nil, err
			}
			optionsList = append(optionsList, optionsEntry)
		}
	}
	sort.Slice(optionsList, func(i, j int) bool {
		return optionsList[i].path < optionsList[j].path
	})
	return optionsList, nil
}

func readConfigAndMerge() (option.Options, error) {
	optionsList, err := readConfig()
	if err != nil {
		return option.Options{}, err
	}
	return mergeOptionsList(optionsList)
}

func mergeOptionsList(optionsList []*OptionsEntry) (option.Options, error) {
	if len(optionsList) == 1 {
		return optionsList[0].options, nil
	}
	var (
		mergedMessage json.RawMessage
		err           error
	)
	for _, options := range optionsList {
		mergedMessage, err = badjson.MergeJSON(globalCtx, options.options.RawMessage, mergedMessage, false)
		if err != nil {
			return option.Options{}, E.Cause(err, "merge config at ", options.path)
		}
	}
	var mergedOptions option.Options
	err = mergedOptions.UnmarshalJSONContext(globalCtx, mergedMessage)
	if err != nil {
		return option.Options{}, E.Cause(err, "unmarshal merged config")
	}
	return mergedOptions, nil
}

// filterInboundsByPanel keeps only the inbounds referenced by a node's
// InTag in the poet panel config (-p). This lets a single static
// server.json carry a superset of inbounds (e.g. both anytls and
// hysteria2) while only the protocols actually present in panel.json are
// instantiated — the unused ones are never created, so they need no
// certificate and bind no port.
//
// SingR-specific behavior (not in upstream sing-box): be careful on
// resync. It is best-effort and gated on -p being set:
//   - no -p           → no filtering (vanilla sing-box behavior).
//   - panel unreadable → keep all inbounds; POET.Start surfaces the error.
//   - panel has no usable InTag → keep all inbounds.
//   - otherwise        → drop inbounds whose tag is not referenced. If
//     that leaves zero, POET.Start fails with "no Node with InTag found",
//     which is the correct signal that server.json and panel.json disagree.
func filterInboundsByPanel(options option.Options) option.Options {
	if poetConfigPath == "" {
		return options
	}
	panelConfig, err := POET.LoadPanelConfig(poetConfigPath)
	if err != nil || panelConfig == nil {
		return options
	}
	wanted := make(map[string]bool)
	for _, node := range panelConfig.NodesConfig {
		if node != nil && node.InTag != "" {
			wanted[node.InTag] = true
		}
	}
	if len(wanted) == 0 {
		return options
	}
	filtered := make([]option.Inbound, 0, len(options.Inbounds))
	for _, inbound := range options.Inbounds {
		if wanted[inbound.Tag] {
			filtered = append(filtered, inbound)
		}
	}
	options.Inbounds = filtered
	return options
}

// Default certificate file names probed under <panel config dir>/certs when a
// TLS inbound leaves both certificate_path and key_path empty. Order matters:
// the first name is also the one written back (and therefore the one named in
// the startup error) when none of them exists on disk.
var defaultCertificateNames = []string{"default.pem", "default.crt"}

const defaultKeyName = "default.key"

// applyDefaultCertificatePaths points TLS inbounds that carry no certificate at
// <dir of -p>/certs/default.{pem,crt} + default.key, so a freshly installed
// server.json can ship with empty TLS material and still work once the operator
// drops a certificate into the well-known location.
//
// SingR-specific behavior (not in upstream sing-box): be careful on resync.
//
//   - The directory is derived from -p, not -c: poetConfigPath is a single
//     string flag (-c is a list and -C takes directories), and every SingR
//     deployment runs `singr run -c <dir>/server.json -p <dir>/panel.json`.
//     That makes the bare-metal (/etc/singr) and docker (/etc/singr-docker)
//     defaults fall out of one expression, with no build-time constant and no
//     "am I in a container" test.
//   - Gated on -p, like filterInboundsByPanel: no panel config means vanilla
//     sing-box behavior, untouched.
//   - Purely additive: an inbound that already names a certificate is never
//     rewritten, so existing installations need no migration.
//   - Runs after filterInboundsByPanel so that inbounds which will not be
//     instantiated are left alone.
//   - Only fires when the whole certificate story is unset. An explicit
//     certificate/key (inline or path), a certificate provider, ACME, REALITY
//     (which needs no certificate at all) or insecure: true (upstream
//     generates a certificate itself) are all left as the operator wrote them.
//   - The substitution is logged per inbound: an implicit path that appears
//     nowhere in server.json must not also be invisible in the log.
func applyDefaultCertificatePaths(options option.Options) option.Options {
	if poetConfigPath == "" {
		return options
	}
	certDirectory, err := filepath.Abs(filepath.Join(filepath.Dir(poetConfigPath), "certs"))
	if err != nil {
		return options
	}
	for _, inbound := range options.Inbounds {
		tlsWrapper, withTLS := inbound.Options.(option.InboundTLSOptionsWrapper)
		if !withTLS {
			continue
		}
		tlsOptions := tlsWrapper.TakeInboundTLSOptions()
		if tlsOptions == nil || !tlsOptions.Enabled {
			continue
		}
		if !certificateUnset(tlsOptions) {
			continue
		}
		certificatePath := filepath.Join(certDirectory, defaultCertificateNames[0])
		for _, name := range defaultCertificateNames {
			candidate := filepath.Join(certDirectory, name)
			if stat, statErr := os.Stat(candidate); statErr == nil && !stat.IsDir() {
				certificatePath = candidate
				break
			}
		}
		keyPath := filepath.Join(certDirectory, defaultKeyName)
		tlsOptions.CertificatePath = certificatePath
		tlsOptions.KeyPath = keyPath
		log.Info("inbound/", inbound.Type, "[", inbound.Tag, "]: no TLS certificate configured, using default ", certificatePath, " + ", keyPath)
	}
	return options
}

// certificateUnset reports whether an inbound leaves the certificate entirely
// to us: no inline material, no paths, no provider/ACME, not REALITY, and not
// asking upstream to generate one via insecure.
func certificateUnset(tlsOptions *option.InboundTLSOptions) bool {
	if tlsOptions.CertificatePath != "" || tlsOptions.KeyPath != "" {
		return false
	}
	if len(tlsOptions.Certificate) > 0 || len(tlsOptions.Key) > 0 {
		return false
	}
	if tlsOptions.CertificateProvider != nil || tlsOptions.ACME != nil {
		return false
	}
	if tlsOptions.Reality != nil && tlsOptions.Reality.Enabled {
		return false
	}
	return !tlsOptions.Insecure
}

func create(options option.Options) (*box.Box, context.CancelFunc, error) {
	options = filterInboundsByPanel(options)
	options = applyDefaultCertificatePaths(options)
	if disableColor {
		if options.Log == nil {
			options.Log = &option.LogOptions{}
		}
		options.Log.DisableColor = true
	}
	ctx, cancel := context.WithCancel(globalCtx)
	instance, err := box.New(box.Options{
		Context:                    ctx,
		Options:                    options,
		NetworkNamespaceHolderArgs: []string{"/proc/self/exe", commandNetnsHolder.Use},
	})
	if err != nil {
		cancel()
		return nil, nil, E.Cause(err, "create service")
	}

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer func() {
		signal.Stop(osSignals)
		close(osSignals)
	}()
	startCtx, finishStart := context.WithCancel(context.Background())
	go func() {
		_, loaded := <-osSignals
		if loaded {
			cancel()
			closeMonitor(startCtx)
		}
	}()
	err = instance.Start()
	finishStart()
	if err != nil {
		cancel()
		return nil, nil, E.Cause(err, "start service")
	}
	return instance, cancel, nil
}

func run() error {
	optionsList, err := readConfig()
	if err != nil {
		return err
	}
	options, err := mergeOptionsList(optionsList)
	if err != nil {
		return err
	}
	err = runInUserNamespaceIfNeeded(options, optionsList)
	if err != nil {
		return err
	}
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(osSignals)
	for {
		instance, cancel, createErr := create(options)
		if createErr != nil {
			return createErr
		}
		runtimeDebug.FreeOSMemory()
		for {
			osSignal := <-osSignals
			if osSignal == syscall.SIGHUP {
				err = check()
				if err != nil {
					log.Error(E.Cause(err, "reload service"))
					continue
				}
			}
			cancel()
			closeCtx, closed := context.WithCancel(context.Background())
			go closeMonitor(closeCtx)
			err = instance.Close()
			closed()
			if osSignal != syscall.SIGHUP {
				if err != nil {
					log.Error(E.Cause(err, "sing-box did not closed properly"))
				}
				return nil
			}
			break
		}
		options, err = readConfigAndMerge()
		if err != nil {
			return err
		}
	}
}

func closeMonitor(ctx context.Context) {
	time.Sleep(C.FatalStopTimeout)
	select {
	case <-ctx.Done():
		return
	default:
	}
	log.Fatal("sing-box did not close!")
}
