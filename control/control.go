package control

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/intelsdi-x/gomit"

	"github.com/intelsdi-x/pulse/control/plugin"
	"github.com/intelsdi-x/pulse/control/plugin/client"
	"github.com/intelsdi-x/pulse/control/routing"
	"github.com/intelsdi-x/pulse/core"
	"github.com/intelsdi-x/pulse/core/cdata"
	"github.com/intelsdi-x/pulse/core/control_event"
	"github.com/intelsdi-x/pulse/core/ctypes"
	"github.com/intelsdi-x/pulse/core/perror"
	"github.com/intelsdi-x/pulse/pkg/psigning"
)

// control private key (RSA private key)
// control public key (RSA public key)
// Plugin token = token generated by plugin and passed to control
// Session token = plugin seed encrypted by control private key, verified by plugin using control public key
//

var (
	controlLogger = log.WithFields(log.Fields{
		"_module": "control",
	})

	ErrLoadedPluginNotFound = errors.New("Loaded plugin not found")
	ErrControllerNotStarted = errors.New("Must start Controller before calling Load()")
)

type executablePlugins []plugin.ExecutablePlugin

type pluginControl struct {
	// TODO, going to need coordination on changing of these
	RunningPlugins executablePlugins
	Started        bool

	autodiscoverPaths []string
	controlPrivKey    *rsa.PrivateKey
	controlPubKey     *rsa.PublicKey
	eventManager      *gomit.EventController

	pluginManager  managesPlugins
	metricCatalog  catalogsMetrics
	pluginRunner   runsPlugins
	signingManager managesSigning

	pluginTrust int
	keyringFile string
}

type runsPlugins interface {
	Start() error
	Stop() []error
	AvailablePlugins() *availablePlugins
	AddDelegates(...gomit.Delegator)
	SetEmitter(gomit.Emitter)
	SetMetricCatalog(catalogsMetrics)
	SetPluginManager(managesPlugins)
	SetStrategy(RoutingStrategy)
	Strategy() RoutingStrategy
	Monitor() *monitor
}

type managesPlugins interface {
	teardown()
	get(string) (*loadedPlugin, error)
	all() map[string]*loadedPlugin
	LoadPlugin(string, gomit.Emitter) (*loadedPlugin, perror.PulseError)
	UnloadPlugin(core.Plugin) (*loadedPlugin, perror.PulseError)
	SetMetricCatalog(catalogsMetrics)
	GenerateArgs(pluginPath string) plugin.Arg
}

type catalogsMetrics interface {
	Get([]string, int) (*metricType, perror.PulseError)
	Add(*metricType)
	AddLoadedMetricType(*loadedPlugin, core.Metric)
	RmUnloadedPluginMetrics(lp *loadedPlugin)
	Fetch([]string) ([]*metricType, perror.PulseError)
	Item() (string, []*metricType)
	Next() bool
	Subscribe([]string, int) perror.PulseError
	Unsubscribe([]string, int) perror.PulseError
	GetPlugin([]string, int) (*loadedPlugin, perror.PulseError)
}

type managesSigning interface {
	ValidateSignature(keyringFile string, signedFile string, signatureFile string) perror.PulseError
}

type controlOpt func(*pluginControl)

func MaxRunningPlugins(m int) controlOpt {
	return func(c *pluginControl) {
		maximumRunningPlugins = m
	}
}

func CacheExpiration(t time.Duration) controlOpt {
	return func(c *pluginControl) {
		client.CacheExpiration = t
	}
}

// New returns a new pluginControl instance
func New(opts ...controlOpt) *pluginControl {

	c := &pluginControl{}
	// Initialize components
	//
	// Event Manager
	c.eventManager = gomit.NewEventController()

	controlLogger.WithFields(log.Fields{
		"_block": "new",
	}).Debug("pevent controller created")

	// Metric Catalog
	c.metricCatalog = newMetricCatalog()
	controlLogger.WithFields(log.Fields{
		"_block": "new",
	}).Debug("metric catalog created")

	// Plugin Manager
	c.pluginManager = newPluginManager()
	controlLogger.WithFields(log.Fields{
		"_block": "new",
	}).Debug("plugin manager created")
	// Plugin Manager needs a reference to the metric catalog
	c.pluginManager.SetMetricCatalog(c.metricCatalog)

	// Signing Manager
	c.signingManager = &psigning.SigningManager{}
	controlLogger.WithFields(log.Fields{
		"_block": "new",
	}).Debug("signing manager created")

	// Plugin Runner
	// TODO (danielscottt): handle routing strat changes via events
	c.pluginRunner = newRunner(&routing.RoundRobinStrategy{})
	controlLogger.WithFields(log.Fields{
		"_block": "new",
	}).Debug("runner created")
	c.pluginRunner.AddDelegates(c.eventManager)
	c.pluginRunner.SetEmitter(c.eventManager) // emitter is passed to created availablePlugins
	c.pluginRunner.SetMetricCatalog(c.metricCatalog)
	c.pluginRunner.SetPluginManager(c.pluginManager)
	c.pluginRunner.SetStrategy(&routing.RoundRobinStrategy{})

	// Wire event manager

	// Start stuff
	err := c.pluginRunner.Start()
	if err != nil {
		panic(err)
	}

	// apply options

	// it is important that this happens last, as an option may
	// require that an internal member of c be constructed.
	for _, opt := range opts {
		opt(c)
	}

	return c
}

func (p *pluginControl) Name() string {
	return "control"
}

// Begin handling load, unload, and inventory
func (p *pluginControl) Start() error {
	// Start pluginManager when pluginControl starts
	p.Started = true
	controlLogger.WithFields(log.Fields{
		"_block": "start",
	}).Info("control started")
	return nil
}

func (p *pluginControl) Stop() {
	p.Started = false
	controlLogger.WithFields(log.Fields{
		"_block": "stop",
	}).Info("control stopped")

	// stop runner
	err := p.pluginRunner.Stop()
	if err != nil {
		controlLogger.Error(err)
	}

	// stop running plugins
	for _, rp := range p.RunningPlugins {
		controlLogger.Debug("Stopping running plugin")
		rp.Kill()
	}

	// unload plugins
	p.pluginManager.teardown()
}

// Load is the public method to load a plugin into
// the LoadedPlugins array and issue an event when
// successful.
func (p *pluginControl) Load(path string) (core.CatalogedPlugin, perror.PulseError) {
	f := map[string]interface{}{
		"_block": "load",
		"path":   path,
	}

	//Check plugin signing
	signatureFile := path + ".asc"
	var signed bool
	if p.pluginTrust == 1 || p.pluginTrust == 2 {
		err := p.signingManager.ValidateSignature(p.keyringFile, path, signatureFile)
		if err != nil {
			if p.pluginTrust == 1 {
				return nil, err
			}
			controlLogger.WithFields(f).Error(err)
		} else {
			signed = true
		}
	}

	controlLogger.WithFields(f).Info("plugin load called")
	if !p.Started {
		pe := perror.New(ErrControllerNotStarted)
		pe.SetFields(f)
		controlLogger.WithFields(f).Error(pe)
		return nil, pe
	}

	pl, err := p.pluginManager.LoadPlugin(path, p.eventManager)
	if err != nil {
		return nil, err
	}
	pl.Signed = signed

	// defer sending event
	event := &control_event.LoadPluginEvent{
		Name:    pl.Meta.Name,
		Version: pl.Meta.Version,
		Type:    int(pl.Meta.Type),
		Signed:  pl.Signed,
	}
	defer p.eventManager.Emit(event)
	return pl, nil
}

func (p *pluginControl) Unload(pl core.Plugin) (core.CatalogedPlugin, perror.PulseError) {
	up, err := p.pluginManager.UnloadPlugin(pl)
	if err != nil {
		return nil, err
	}

	event := &control_event.UnloadPluginEvent{
		Name:    up.Meta.Name,
		Version: up.Meta.Version,
		Type:    int(up.Meta.Type),
	}
	defer p.eventManager.Emit(event)
	return up, nil
}

func (p *pluginControl) SwapPlugins(inPath string, out core.CatalogedPlugin) perror.PulseError {

	lp, err := p.pluginManager.LoadPlugin(inPath, p.eventManager)
	if err != nil {
		return err
	}

	up, err := p.pluginManager.UnloadPlugin(out)
	if err != nil {
		_, err2 := p.pluginManager.UnloadPlugin(lp)
		if err2 != nil {
			pe := perror.New(errors.New("failed to rollback after error"))
			pe.SetFields(map[string]interface{}{
				"original-unload-error": err.Error(),
				"rollback-unload-error": err2.Error(),
			})
			return err
		}
		return err
	}

	event := &control_event.SwapPluginsEvent{
		LoadedPluginName:      lp.Meta.Name,
		LoadedPluginVersion:   lp.Meta.Version,
		UnloadedPluginName:    up.Meta.Name,
		UnloadedPluginVersion: up.Meta.Version,
		PluginType:            int(lp.Meta.Type),
	}
	defer p.eventManager.Emit(event)

	return nil
}

func (p *pluginControl) ValidateDeps(mts []core.Metric, plugins []core.SubscribedPlugin) []perror.PulseError {
	var perrs []perror.PulseError
	for _, mt := range mts {
		_, errs := p.validateMetricTypeSubscription(mt, mt.Config())
		if len(errs) > 0 {
			perrs = append(perrs, errs...)
		}
	}
	if len(perrs) > 0 {
		return perrs
	}

	//validate plugins
	for _, plg := range plugins {
		errs := p.validatePluginSubscription(plg)
		if len(errs) > 0 {
			perrs = append(perrs, errs...)
			return perrs
		}
	}

	return perrs
}

func (p *pluginControl) validatePluginSubscription(pl core.SubscribedPlugin) []perror.PulseError {
	var perrs = []perror.PulseError{}
	controlLogger.WithFields(log.Fields{
		"_block": "validate-plugin-subscription",
		"plugin": fmt.Sprintf("%s:%d", pl.Name(), pl.Version()),
	}).Info(fmt.Sprintf("validating dependencies for plugin %s:%d", pl.Name(), pl.Version()))
	lp, err := p.pluginManager.get(fmt.Sprintf("%s:%s:%d", pl.TypeName(), pl.Name(), pl.Version()))
	if err != nil {
		pe := perror.New(fmt.Errorf("Plugin not found: type(%s) name(%s) version(%d)", pl.TypeName(), pl.Name(), pl.Version()))
		pe.SetFields(map[string]interface{}{
			"name":    pl.Name(),
			"version": pl.Version(),
			"type":    pl.TypeName(),
		})
		perrs = append(perrs, pe)
		return perrs
	}

	if lp.ConfigPolicy != nil {
		ncd := lp.ConfigPolicy.Get([]string{""})
		_, errs := ncd.Process(pl.Config().Table())
		if errs != nil && errs.HasErrors() {
			for _, e := range errs.Errors() {
				pe := perror.New(e)
				pe.SetFields(map[string]interface{}{"name": pl.Name(), "version": pl.Version()})
				perrs = append(perrs, pe)
			}
		}
	}
	return perrs
}

func (p *pluginControl) validateMetricTypeSubscription(mt core.RequestedMetric, cd *cdata.ConfigDataNode) (core.Metric, []perror.PulseError) {
	var perrs []perror.PulseError
	controlLogger.WithFields(log.Fields{
		"_block":    "validate-metric-subscription",
		"namespace": mt.Namespace(),
	}).Info("subscription called on metric")

	m, err := p.metricCatalog.Get(mt.Namespace(), mt.Version())
	if err != nil {
		perrs = append(perrs, err)
		return nil, perrs
	}

	// No metric found return error.
	if m == nil {
		perrs = append(perrs, perror.New(errors.New(fmt.Sprintf("no metric found cannot subscribe: (%s) version(%d)", mt.Namespace(), mt.Version()))))
		return nil, perrs
	}
	m.config = cd

	if m.Config() != nil {
		ncdTable, errs := m.policy.Process(m.Config().Table())
		if errs != nil && errs.HasErrors() {
			for _, e := range errs.Errors() {
				perrs = append(perrs, perror.New(e))
			}
			return nil, perrs
		}
		m.config = cdata.FromTable(*ncdTable)
	}

	return m, perrs
}

func (p *pluginControl) gatherCollectors(mts []core.Metric) ([]core.Plugin, []perror.PulseError) {
	var (
		plugins []core.Plugin
		perrs   []perror.PulseError
	)

	// here we resolve and retrieve plugins for each metric type.
	// if the incoming metric type version is < 1, we treat that as
	// latest as with plugins.  The following two loops create a set
	// of plugins with proper versions needed to discern the subscription
	// types.
	colPlugins := make(map[string]*loadedPlugin)
	for _, mt := range mts {
		m, err := p.metricCatalog.Get(mt.Namespace(), mt.Version())
		if err != nil {
			perrs = append(perrs, perror.New(err))
			continue
		}
		// if the metric subscription is to version -1, we need to carry
		// that forward in the subscription.
		if mt.Version() < 1 {
			// make a copy of the loadedPlugin and overwrite the version.
			npl := *m.Plugin
			npl.Meta.Version = -1
			colPlugins[npl.Key()] = &npl
		} else {
			colPlugins[m.Plugin.Key()] = m.Plugin
		}
	}
	if len(perrs) > 0 {
		return plugins, perrs
	}

	for _, lp := range colPlugins {
		plugins = append(plugins, lp)
	}

	return plugins, nil
}

func (p *pluginControl) SubscribeDeps(taskId uint64, mts []core.Metric, plugins []core.Plugin) []perror.PulseError {
	var perrs []perror.PulseError

	collectors, errs := p.gatherCollectors(mts)
	if len(errs) > 0 {
		perrs = append(perrs)
	}
	plugins = append(plugins, collectors...)

	for _, sub := range plugins {
		// pools are created statically, not with keys like "publisher:foo:-1"
		// here we check to see if the version of the incoming plugin is -1, and
		// if it is, we look up the latest in loaded plugins, and use that key to
		// create the pool.
		if sub.Version() < 1 {
			latest, err := p.pluginManager.get(fmt.Sprintf("%s:%s:%d", sub.TypeName(), sub.Name(), sub.Version()))
			if err != nil {
				perrs = append(perrs, perror.New(err))
				return perrs
			}
			pool, err := p.pluginRunner.AvailablePlugins().getOrCreatePool(latest.Key())
			if err != nil {
				perrs = append(perrs, perror.New(err))
				return perrs
			}
			pool.subscribe(taskId, unboundSubscriptionType)
		} else {
			pool, err := p.pluginRunner.AvailablePlugins().getOrCreatePool(fmt.Sprintf("%s:%s:%d", sub.TypeName(), sub.Name(), sub.Version()))
			if err != nil {
				perrs = append(perrs, perror.New(err))
				return perrs
			}
			pool.subscribe(taskId, boundSubscriptionType)
		}
		perr := p.sendPluginSubscriptionEvent(taskId, sub)
		if perr != nil {
			perrs = append(perrs, perr)
		}
	}

	return perrs
}

func (p *pluginControl) sendPluginSubscriptionEvent(taskId uint64, pl core.Plugin) perror.PulseError {
	pt, err := core.ToPluginType(pl.TypeName())
	if err != nil {
		return perror.New(err)
	}
	e := &control_event.PluginSubscriptionEvent{
		TaskId:           taskId,
		PluginType:       int(pt),
		PluginName:       pl.Name(),
		PluginVersion:    pl.Version(),
		SubscriptionType: int(unboundSubscriptionType),
	}
	if pl.Version() > 0 {
		e.SubscriptionType = int(boundSubscriptionType)
	}
	if _, err := p.eventManager.Emit(e); err != nil {
		return perror.New(err)
	}
	return nil
}

func (p *pluginControl) UnsubscribeDeps(taskId uint64, mts []core.Metric, plugins []core.Plugin) []perror.PulseError {
	var perrs []perror.PulseError

	collectors, errs := p.gatherCollectors(mts)
	if len(errs) > 0 {
		perrs = append(perrs, errs...)
	}
	plugins = append(plugins, collectors...)

	for _, sub := range plugins {
		pool, err := p.pluginRunner.AvailablePlugins().getPool(fmt.Sprintf("%s:%s:%d", sub.TypeName(), sub.Name(), sub.Version()))
		if err != nil {
			perrs = append(perrs, err)
			return perrs
		}
		if pool != nil {
			pool.unsubscribe(taskId)
		}
		perr := p.sendPluginUnsubscriptionEvent(taskId, sub)
		if perr != nil {
			perrs = append(perrs, perr)
		}
	}

	return perrs
}

func (p *pluginControl) sendPluginUnsubscriptionEvent(taskId uint64, pl core.Plugin) perror.PulseError {
	pt, err := core.ToPluginType(pl.TypeName())
	if err != nil {
		return perror.New(err)
	}
	e := &control_event.PluginUnsubscriptionEvent{
		TaskId:        taskId,
		PluginType:    int(pt),
		PluginName:    pl.Name(),
		PluginVersion: pl.Version(),
	}
	if _, err := p.eventManager.Emit(e); err != nil {
		return perror.New(err)
	}
	return nil
}

// SetMonitorOptions exposes monitors options
func (p *pluginControl) SetMonitorOptions(options ...monitorOption) {
	p.pluginRunner.Monitor().Option(options...)
}

// returns the loaded plugin collection
// NOTE: The returned data from this function should be considered constant and read only
func (p *pluginControl) PluginCatalog() core.PluginCatalog {
	table := p.pluginManager.all()
	plugins := make([]core.CatalogedPlugin, len(table))
	i := 0
	for _, plugin := range table {
		plugins[i] = plugin
		i++
	}
	return plugins
}

// AvailablePlugins returns pointers to all the running plugins in the pools
// NOTE: The returned data from this function should be considered constant and read only
func (p *pluginControl) AvailablePlugins() []core.AvailablePlugin {
	var caps []core.AvailablePlugin
	for _, ap := range p.pluginRunner.AvailablePlugins().all() {
		caps = append(caps, ap)
	}
	return caps
}

// MetricCatalog returns the entire metric catalog
// NOTE: The returned data from this function should be considered constant and read only
func (p *pluginControl) MetricCatalog() ([]core.CatalogedMetric, error) {
	return p.FetchMetrics([]string{}, 0)
}

// FetchMetrics returns the metrics which fall under the given namespace
// NOTE: The returned data from this function should be considered constant and read only
func (p *pluginControl) FetchMetrics(ns []string, version int) ([]core.CatalogedMetric, error) {
	mts, err := p.metricCatalog.Fetch(ns)
	if err != nil {
		return nil, err
	}
	cmt := make([]core.CatalogedMetric, 0, len(mts))
	for _, mt := range mts {
		if version > 0 {
			if mt.version == version {
				cmt = append(cmt, mt)
			}
		} else {
			cmt = append(cmt, mt)
		}
	}
	return cmt, nil
}

func (p *pluginControl) GetMetric(ns []string, ver int) (core.CatalogedMetric, error) {
	return p.metricCatalog.Get(ns, ver)
}

func (p *pluginControl) MetricExists(mns []string, ver int) bool {
	_, err := p.metricCatalog.Get(mns, ver)
	if err == nil {
		return true
	}
	return false
}

// CollectMetrics is a blocking call to collector plugins returning a collection
// of metrics and errors.  If an error is encountered no metrics will be
// returned.
func (p *pluginControl) CollectMetrics(metricTypes []core.Metric, deadline time.Time) (metrics []core.Metric, errs []error) {

	pluginToMetricMap, err := groupMetricTypesByPlugin(p.metricCatalog, metricTypes)
	if err != nil {
		errs = append(errs, err)
		return
	}

	cMetrics := make(chan []core.Metric)
	cError := make(chan error)
	var wg sync.WaitGroup

	// For each available plugin call available plugin using RPC client and wait for response (goroutines)
	for pluginKey, pmt := range pluginToMetricMap {

		// retrieve an available plugin
		pool, err := p.pluginRunner.AvailablePlugins().holdPool(pluginKey)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if pool != nil {
			defer pool.release()

			ap, err := pool.selectAP(p.pluginRunner.Strategy())
			if err != nil {
				errs = append(errs, err)
				continue
			}

			// cast client to PluginCollectorClient
			cli, ok := ap.client.(client.PluginCollectorClient)
			if !ok {
				err := errors.New("unable to cast client to PluginCollectorClient")
				errs = append(errs, err)
				continue
			}

			wg.Add(1)

			// get a metrics
			go func(mt []core.Metric) {
				mts, err := cli.CollectMetrics(mt)
				if err != nil {
					cError <- err
				} else {
					cMetrics <- mts
				}
			}(pmt.metricTypes)

			// update statics about plugin
			ap.hitCount++
			ap.lastHitTime = time.Now()
		} else {
			errs = append(errs, fmt.Errorf("pool not found for plugin key: %s", pluginKey))
		}
	}

	go func() {
		for m := range cMetrics {
			metrics = append(metrics, m...)
			wg.Done()
		}
	}()

	go func() {
		for e := range cError {
			errs = append(errs, e)
			wg.Done()
		}
	}()

	wg.Wait()
	close(cMetrics)
	close(cError)

	if len(errs) > 0 {
		return nil, errs
	}
	return
}

// PublishMetrics
func (p *pluginControl) PublishMetrics(contentType string, content []byte, pluginName string, pluginVersion int, config map[string]ctypes.ConfigValue) []error {
	var errs []error
	key := strings.Join([]string{"publisher", pluginName, strconv.Itoa(pluginVersion)}, ":")

	// retrieve an available plugin
	pool, err := p.pluginRunner.AvailablePlugins().holdPool(key)
	if err != nil {
		errs = append(errs, err)
		return errs
	}
	if pool != nil {
		defer pool.release()

		ap, err := pool.selectAP(p.pluginRunner.Strategy())
		if err != nil {
			errs = append(errs, err)
			return errs
		}

		cli, ok := ap.client.(client.PluginPublisherClient)
		if !ok {
			return []error{errors.New("unable to cast client to PluginPublisherClient")}
		}

		errp := cli.Publish(contentType, content, config)
		if err != nil {
			return []error{errp}
		}
		ap.hitCount++
		ap.lastHitTime = time.Now()
		return nil
	}
	return []error{errors.New("pool not found")}
}

// ProcessMetrics
func (p *pluginControl) ProcessMetrics(contentType string, content []byte, pluginName string, pluginVersion int, config map[string]ctypes.ConfigValue) (string, []byte, []error) {
	var errs []error
	key := strings.Join([]string{"processor", pluginName, strconv.Itoa(pluginVersion)}, ":")

	// retrieve an available plugin
	pool, err := p.pluginRunner.AvailablePlugins().holdPool(key)
	if err != nil {
		errs = append(errs, err)
		return "", nil, errs
	}
	if pool != nil {
		defer pool.release()

		ap, err := pool.selectAP(p.pluginRunner.Strategy())
		if err != nil {
			errs = append(errs, err)
			return "", nil, errs
		}

		cli, ok := ap.client.(client.PluginProcessorClient)
		if !ok {
			return "", nil, []error{errors.New("unable to cast client to PluginProcessorClient")}
		}

		ct, c, errp := cli.Process(contentType, content, config)
		if err != nil {
			return "", nil, []error{errp}
		}
		ap.hitCount++
		ap.lastHitTime = time.Now()
		return ct, c, nil
	}
	return "", nil, []error{errors.New("pool not found")}
}

// GetPluginContentTypes returns accepted and returned content types for the
// loaded plugin matching the provided name, type and version.
// If the version provided is 0 or less the newest plugin by version will be
// returned.
func (p *pluginControl) GetPluginContentTypes(n string, t core.PluginType, v int) ([]string, []string, error) {
	lp, err := p.pluginManager.get(fmt.Sprintf("%s:%s:%d", t.String(), n, v))
	if err != nil {
		return nil, nil, err
	}
	return lp.Meta.AcceptedContentTypes, lp.Meta.ReturnedContentTypes, nil
}

func (p *pluginControl) SetAutodiscoverPaths(paths []string) {
	p.autodiscoverPaths = paths
}

func (p *pluginControl) GetAutodiscoverPaths() []string {
	return p.autodiscoverPaths
}

func (p *pluginControl) SetPluginTrustLevel(trust int) {
	p.pluginTrust = trust
}

func (p *pluginControl) SetKeyringFile(keyring string) {
	p.keyringFile = keyring
}

type requestedPlugin struct {
	name    string
	version int
	config  *cdata.ConfigDataNode
}

func (r *requestedPlugin) Name() string {
	return r.name
}

func (r *requestedPlugin) Version() int {
	return r.version
}

func (r *requestedPlugin) Config() *cdata.ConfigDataNode {
	return r.config
}

// ------------------- helper struct and function for grouping metrics types ------

// just a tuple of loadedPlugin and metricType slice
type pluginMetricTypes struct {
	plugin      *loadedPlugin
	metricTypes []core.Metric
}

func (p *pluginMetricTypes) Count() int {
	return len(p.metricTypes)
}

// groupMetricTypesByPlugin groups metricTypes by a plugin.Key() and returns appropriate structure
func groupMetricTypesByPlugin(cat catalogsMetrics, metricTypes []core.Metric) (map[string]pluginMetricTypes, error) {
	pmts := make(map[string]pluginMetricTypes)
	// For each plugin type select a matching available plugin to call
	for _, mt := range metricTypes {

		// This is set to choose the newest and not pin version. TODO, be sure version is set to -1 if not provided by user on Task creation.
		lp, err := cat.GetPlugin(mt.Namespace(), -1)
		if err != nil {
			return nil, err
		}
		// if loaded plugin is nil, we have failed.  return error
		if lp == nil {
			return nil, errorMetricNotFound(mt.Namespace())
		}

		key := lp.Key()

		//
		pmt, _ := pmts[key]
		pmt.plugin = lp
		pmt.metricTypes = append(pmt.metricTypes, mt)
		pmts[key] = pmt

	}
	return pmts, nil
}
