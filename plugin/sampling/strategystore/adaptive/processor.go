package adaptive

import (
	"errors"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/jaegertracing/jaeger/thrift-gen/sampling"
	"github.com/uber-go/atomic"
	"github.com/uber/jaeger-lib/metrics"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger/pkg/distributedlock"
	"github.com/jaegertracing/jaeger/plugin/sampling/strategystore/adaptive/calculationstrategy"
	"github.com/jaegertracing/jaeger/storage/samplingstore"
)

const (
	maxSamplingProbability = 1.0

	samplingLock = "sampling_lock"

	getThroughputErrMsg = "Failed to get throughput from storage"
	acquireLockErrMsg   = "Failed to acquire lock"

	defaultFollowerProbabilityInterval = 20 * time.Second

	// The number of past entries for samplingCache the leader keeps in memory
	serviceCacheSize = 25
)

var (
	errIntervals        = errors.New("CalculationInterval must be less than LookbackInterval")
	errNonZeroIntervals = errors.New("CalculationInterval and LookbackInterval must be greater than 0")
	errLockIntervals    = errors.New("FollowerLeaseRefreshInterval cannot be less than LeaderLeaseRefreshInterval")
	errLookbackQPSCount = errors.New("LookbackQPSCount cannot be less than 1")
)

type serviceOperationThroughput map[string]map[string]*samplingstore.Throughput

type throughputBucket struct {
	throughput serviceOperationThroughput
	interval   time.Duration
	endTime    time.Time
}

// Processor retrieves service throughput over a look back interval and calculates sampling probabilities
// per operation such that each operation is sampled at a specified target QPS. It achieves this by
// retrieving discrete buckets of operation throughput and doing a weighted average of the throughput
// and generating a probability to match the targetQPS.
type Processor interface {
	// GetSamplingStrategyResponses returns the sampling strategy response probabilities of a service.
	GetSamplingStrategyResponses(service string) sampling.SamplingStrategyResponse

	// Start initializes and starts the sampling processor which regularly calculates sampling probabilities.
	Start()

	// Stop stops the processor from calculating probabilities.
	Stop()
}

type processor struct {
	sync.RWMutex
	ProcessorConfig

	// flag used to determine if this processor has the leader lock.
	atomic.Bool

	storage                 samplingstore.Store
	lock                    distributedlock.Lock
	acquireLockStop         chan struct{}
	calculationStop         chan struct{}
	updateProbabilitiesStop chan struct{}
	hostname                string

	// buckets is the number of `calculationInterval` buckets used to calculate the probabilities.
	// It is calculated as lookbackInterval / calculationInterval.
	buckets int

	// probabilities contains the latest calculated sampling probabilities for service operations.
	probabilities samplingstore.ServiceOperationProbabilities

	// qps contains the latest calculated qps for service operations; the calculation is essentially
	// throughput / CalculationInterval.
	qps samplingstore.ServiceOperationQPS

	// throughputs slice of `buckets` size that stores the aggregated throughput. The latest throughput
	// is stored at the head of the slice.
	throughputs []*throughputBucket

	// strategyResponses contains the sampling strategies for every service.
	strategyResponses map[string]*sampling.SamplingStrategyResponse

	logger *zap.Logger

	weightsCache *weightsCache

	probabilityCalculator calculationstrategy.ProbabilityCalculator

	// followerProbabilityInterval determines how often the follower processor updates its probabilities.
	// Given only the leader writes probabilities, the followers need to fetch the probabilities into
	// cache.
	followerProbabilityInterval time.Duration

	serviceCache []samplingCache

	operationsCalculatedGauge     metrics.Gauge
	calculateProbabilitiesLatency metrics.Timer
}

// NewProcessor creates a new sampling processor that generates sampling rates for service operations
func NewProcessor(
	config ProcessorConfig,
	hostname string,
	storage samplingstore.Store,
	lock distributedlock.Lock,
	metricsFactory metrics.Factory,
	logger *zap.Logger,
) (Processor, error) {
	if config.LookbackInterval < config.CalculationInterval {
		return nil, errIntervals
	}
	if config.CalculationInterval == 0 || config.LookbackInterval == 0 {
		return nil, errNonZeroIntervals
	}
	if config.FollowerLeaseRefreshInterval < config.LeaderLeaseRefreshInterval {
		return nil, errLockIntervals
	}
	if config.LookbackQPSCount < 1 {
		return nil, errLookbackQPSCount
	}
	buckets := int(config.LookbackInterval / config.CalculationInterval)
	metricsFactory = metricsFactory.Namespace("adaptive_sampling_processor", nil)
	return &processor{
		ProcessorConfig:   config,
		storage:           storage,
		buckets:           buckets,
		probabilities:     make(samplingstore.ServiceOperationProbabilities),
		qps:               make(samplingstore.ServiceOperationQPS),
		hostname:          hostname,
		strategyResponses: make(map[string]*sampling.SamplingStrategyResponse),
		logger:            logger,
		lock:              lock,
		// TODO make weightsCache and probabilityCalculator configurable
		weightsCache:                  newWeightsCache(),
		probabilityCalculator:         calculationstrategy.NewPercentageIncreaseCappedCalculator(1.0),
		followerProbabilityInterval:   defaultFollowerProbabilityInterval,
		serviceCache:                  []samplingCache{},
		operationsCalculatedGauge:     metricsFactory.Gauge("operations_calculated", nil),
		calculateProbabilitiesLatency: metricsFactory.Timer("calculate_probabilities", nil),
	}, nil
}

func (p *processor) GetSamplingStrategyResponses(service string) sampling.SamplingStrategyResponse {
	p.RLock()
	defer p.RUnlock()
	if strategy, ok := p.strategyResponses[service]; ok {
		return *strategy
	}
	return p.generateDefaultSamplingStrategyResponse()
}

func (p *processor) Start() {
	p.logger.Info("Starting sampling processor")
	p.acquireLockStop = make(chan struct{})
	p.calculationStop = make(chan struct{})
	p.updateProbabilitiesStop = make(chan struct{})
	p.setLeader(false)
	p.loadProbabilities()
	p.generateStrategyResponses()
	go p.runAcquireLockLoop()
	go p.runCalculationLoop()
	go p.runUpdateProbabilitiesLoop()
}

func (p *processor) Stop() {
	p.logger.Info("Stopping sampling processor")
	close(p.acquireLockStop)
	close(p.calculationStop)
	close(p.updateProbabilitiesStop)
}

func (p *processor) loadProbabilities() {
	// TODO GetLatestProbabilities API can be changed to return the latest measured qps for initialization
	probabilities, err := p.storage.GetLatestProbabilities()
	if err != nil {
		p.logger.Warn("Failed to initialize probabilities", zap.Error(err))
		return
	}
	p.Lock()
	defer p.Unlock()
	p.probabilities = probabilities
}

// runAcquireLockLoop attempts to acquire the leader lock. If it succeeds, it will attempt to retain it,
// otherwise it sleeps and attempts to gain the lock again.
func (p *processor) runAcquireLockLoop() {
	addJitter(p.LeaderLeaseRefreshInterval)
	ticker := time.NewTicker(p.acquireLock())
	for {
		select {
		case <-ticker.C:
			ticker.Stop()
			ticker = time.NewTicker(p.acquireLock())
		case <-p.acquireLockStop:
			ticker.Stop()
			return
		}
	}
}

// acquireLock attempts to acquire the lock and returns the interval to sleep before the next retry.
func (p *processor) acquireLock() time.Duration {
	if acquiredLeaderLock, err := p.lock.Acquire(samplingLock); err == nil {
		p.setLeader(acquiredLeaderLock)
	} else {
		p.logger.Error(acquireLockErrMsg, zap.Error(err))
	}
	if p.isLeader() {
		// If this host holds the leader lock, retry with a shorter cadence
		// to retain the leader lease.
		return p.LeaderLeaseRefreshInterval
	}
	// If this host failed to acquire the leader lock, retry with a longer cadence
	return p.FollowerLeaseRefreshInterval
}

// runUpdateProbabilitiesLoop starts a loop that reads probabilities from storage.
// The follower updates its local cache with the latest probabilities and serves them.
func (p *processor) runUpdateProbabilitiesLoop() {
	addJitter(p.followerProbabilityInterval)
	ticker := time.NewTicker(p.followerProbabilityInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// Only load probabilities if this processor doesn't hold the leader lock
			if !p.isLeader() {
				p.loadProbabilities()
				p.generateStrategyResponses()
			}
		case <-p.updateProbabilitiesStop:
			return
		}
	}
}

func (p *processor) isLeader() bool {
	return p.Load()
}

func (p *processor) setLeader(isLeader bool) {
	p.Store(isLeader)
}

// addJitter sleeps for a random amount of time. Without jitter, if the host holding the leader
// lock were to die, then all other collectors can potentially wait for a full cycle before
// trying to acquire the lock. With jitter, we can reduce the average amount of time before a
// new leader is elected. Furthermore, jitter can be used to spread out read load on storage.
func addJitter(jitterAmount time.Duration) {
	randomTime := (jitterAmount / 2) + time.Duration(rand.Int63n(int64(jitterAmount/2)))
	time.Sleep(randomTime)
}

func (p *processor) runCalculationLoop() {
	lastCheckedTime := time.Now().Add(p.Delay * -1)
	p.initializeThroughput(lastCheckedTime)
	// NB: the first tick will be slightly delayed by the initializeThroughput call.
	ticker := time.NewTicker(p.CalculationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			endTime := time.Now().Add(p.Delay * -1)
			startTime := lastCheckedTime
			throughput, err := p.storage.GetThroughput(startTime, endTime)
			if err != nil {
				p.logger.Error(getThroughputErrMsg, zap.Error(err))
				break
			}
			aggregatedThroughput := p.aggregateThroughput(throughput)
			p.prependThroughputBucket(&throughputBucket{
				throughput: aggregatedThroughput,
				interval:   endTime.Sub(startTime),
				endTime:    endTime,
			})
			lastCheckedTime = endTime
			// Load the latest throughput so that if this host ever becomes leader, it
			// has the throughput ready in memory. However, only run the actual calculations
			// if this host becomes leader.
			// TODO fill the throughput buffer only when we're leader
			if p.isLeader() {
				startTime := time.Now()
				probabilities, qps := p.calculateProbabilitiesAndQPS()
				p.Lock()
				p.probabilities = probabilities
				p.qps = qps
				p.Unlock()
				// NB: This has the potential of running into a race condition if the calculationInterval
				// is set to an extremely low value. The worst case scenario is that probabilities is calculated
				// and swapped more than once before generateStrategyResponses() and saveProbabilities() are called.
				// This will result in one or more batches of probabilities not being saved which is completely
				// fine. This race condition should not ever occur anyway since the calculation interval will
				// be way longer than the time to run the calculations.
				p.generateStrategyResponses()
				p.calculateProbabilitiesLatency.Record(time.Now().Sub(startTime))
				go p.saveProbabilitiesAndQPS()
			}
		case <-p.calculationStop:
			return
		}
	}
}

func (p *processor) saveProbabilitiesAndQPS() {
	p.RLock()
	defer p.RUnlock()
	if err := p.storage.InsertProbabilitiesAndQPS(p.hostname, p.probabilities, p.qps); err != nil {
		p.logger.Warn("Could not save probabilities", zap.Error(err))
	}
}

func (p *processor) prependThroughputBucket(bucket *throughputBucket) {
	p.throughputs = append([]*throughputBucket{bucket}, p.throughputs...)
	if len(p.throughputs) > p.buckets {
		p.throughputs = p.throughputs[0:p.buckets]
	}
}

// aggregateThroughput aggregates operation throughput from different buckets into one.
func (p *processor) aggregateThroughput(throughputs []*samplingstore.Throughput) serviceOperationThroughput {
	aggregatedThroughput := make(serviceOperationThroughput)
	for _, throughput := range throughputs {
		service := throughput.Service
		operation := throughput.Operation
		if _, ok := aggregatedThroughput[service]; !ok {
			aggregatedThroughput[service] = make(map[string]*samplingstore.Throughput)
		}
		if t, ok := aggregatedThroughput[service][operation]; ok {
			t.Count += throughput.Count
			t.Probabilities = combineProbabilities(t.Probabilities, throughput.Probabilities)
		} else {
			aggregatedThroughput[service][operation] = throughput
		}
	}
	return aggregatedThroughput
}

func (p *processor) initializeThroughput(endTime time.Time) {
	for i := 0; i < p.buckets; i++ {
		startTime := endTime.Add(p.CalculationInterval * -1)
		throughput, err := p.storage.GetThroughput(startTime, endTime)
		if err != nil && p.logger != nil {
			p.logger.Error(getThroughputErrMsg, zap.Error(err))
			return
		}
		if len(throughput) == 0 {
			return
		}
		aggregatedThroughput := p.aggregateThroughput(throughput)
		p.throughputs = append(p.throughputs, &throughputBucket{
			throughput: aggregatedThroughput,
			interval:   p.CalculationInterval,
			endTime:    endTime,
		})
		endTime = startTime
	}
}

type serviceOperationQPS map[string]map[string][]float64

func (p *processor) generateOperationQPS() serviceOperationQPS {
	// TODO previous qps buckets have already been calculated, just need to calculate latest batch and append them
	// where necessary and throw out the oldest batch. Edge case #buckets < p.buckets, then we shouldn't throw out
	qps := make(serviceOperationQPS)
	for _, bucket := range p.throughputs {
		for svc, operations := range bucket.throughput {
			if _, ok := qps[svc]; !ok {
				qps[svc] = make(map[string][]float64)
			}
			for op, throughput := range operations {
				if len(qps[svc][op]) >= p.LookbackQPSCount {
					continue
				}
				qps[svc][op] = append(qps[svc][op], calculateQPS(throughput.Count, bucket.interval))
			}
		}
	}
	return qps
}

func calculateQPS(count int64, interval time.Duration) float64 {
	seconds := float64(interval) / float64(time.Second)
	return float64(count) / seconds
}

// calculateWeightedQPS calculates the weighted qps of the slice allQPS where weights are biased towards more recent
// qps. This function assumes that the most recent qps is at the head of the slice.
func (p *processor) calculateWeightedQPS(allQPS []float64) float64 {
	if len(allQPS) == 0 {
		return 0
	}
	weights := p.weightsCache.getWeights(len(allQPS))
	var qps float64
	for i := 0; i < len(allQPS); i++ {
		qps += allQPS[i] * weights[i]
	}
	return qps
}

func (p *processor) prependServiceCache() {
	p.serviceCache = append([]samplingCache{make(samplingCache)}, p.serviceCache...)
	if len(p.serviceCache) > serviceCacheSize {
		p.serviceCache = p.serviceCache[0:serviceCacheSize]
	}
}

func (p *processor) calculateProbabilitiesAndQPS() (samplingstore.ServiceOperationProbabilities, samplingstore.ServiceOperationQPS) {
	p.prependServiceCache()
	retProbabilities := make(samplingstore.ServiceOperationProbabilities)
	retQPS := make(samplingstore.ServiceOperationQPS)
	svcOpQPS := p.generateOperationQPS()
	totalOperations := int64(0)
	for svc, opQPS := range svcOpQPS {
		if _, ok := retProbabilities[svc]; !ok {
			retProbabilities[svc] = make(map[string]float64)
		}
		if _, ok := retQPS[svc]; !ok {
			retQPS[svc] = make(map[string]float64)
		}
		for op, qps := range opQPS {
			totalOperations++
			avgQPS := p.calculateWeightedQPS(qps)
			retQPS[svc][op] = avgQPS
			retProbabilities[svc][op] = p.calculateProbability(svc, op, avgQPS)
		}
	}
	p.operationsCalculatedGauge.Update(totalOperations)
	return retProbabilities, retQPS
}

func (p *processor) calculateProbability(service, operation string, qps float64) float64 {
	oldProbability := p.DefaultSamplingProbability
	// TODO: is this loop overly expensive?
	p.RLock()
	if opProbabilities, ok := p.probabilities[service]; ok {
		if probability, ok := opProbabilities[operation]; ok {
			oldProbability = probability
		}
	}
	latestThroughput := p.throughputs[0].throughput
	p.RUnlock()

	usingAdaptiveSampling := p.usingAdaptiveSampling(oldProbability, service, operation, latestThroughput)
	p.serviceCache[0].Set(service, operation, &samplingCacheEntry{
		probability:    oldProbability,
		usingAdapative: usingAdaptiveSampling,
	})

	// Short circuit if the qps is close enough to targetQPS or if the service isn't using
	// adaptive sampling.
	targetQPS := p.Mutable.GetTargetQPS()
	if math.Abs(qps-targetQPS) < p.Mutable.GetQPSEquivalenceThreshold() || !usingAdaptiveSampling {
		return oldProbability
	}
	var newProbability float64
	if floatEquals(qps, 0) {
		// Edge case, we double the sampling probability if the QPS is 0 so that we force the service
		// to at least sample one span probabilistically
		newProbability = oldProbability * 2.0
	} else {
		newProbability = p.probabilityCalculator.Calculate(targetQPS, qps, oldProbability)
	}
	return math.Min(maxSamplingProbability, math.Max(p.MinSamplingProbability, newProbability))
}

func combineProbabilities(p1 map[string]struct{}, p2 map[string]struct{}) map[string]struct{} {
	for probability := range p2 {
		p1[probability] = struct{}{}
	}
	return p1
}

func (p *processor) usingAdaptiveSampling(probability float64, service, operation string, throughput serviceOperationThroughput) bool {
	if floatEquals(probability, p.DefaultSamplingProbability) {
		// If the service is seen for the first time, assume it's using adaptive sampling (ie prob == defaultProb).
		// Even if this isn't the case, the next time around this loop, the newly calculated probability will not equal
		// the defaultProb so the logic will fall through.
		return true
	}
	var opThroughput *samplingstore.Throughput
	svcThroughput, ok := throughput[service]
	if ok {
		opThroughput = svcThroughput[operation]
	}
	if opThroughput != nil {
		f := truncateFloat(probability)
		_, ok = opThroughput.Probabilities[f]
		return ok
	}
	// By this point, we know that there's no recorded throughput for this operation for this round
	// of calculation. Check the previous bucket to see if this operation was using adaptive sampling
	// before.
	if len(p.serviceCache) > 1 {
		if e := p.serviceCache[1].Get(service, operation); e != nil {
			return e.usingAdapative && e.probability != p.DefaultSamplingProbability
		}
	}
	return false
}

// generateStrategyResponses generates a SamplingStrategyResponse from the calculated sampling probabilities.
func (p *processor) generateStrategyResponses() {
	p.RLock()
	strategies := make(map[string]*sampling.SamplingStrategyResponse)
	for svc, opProbabilities := range p.probabilities {
		opStrategies := make([]*sampling.OperationSamplingStrategy, len(opProbabilities))
		var idx int
		for op, probability := range opProbabilities {
			opStrategies[idx] = &sampling.OperationSamplingStrategy{
				Operation: op,
				ProbabilisticSampling: &sampling.ProbabilisticSamplingStrategy{
					SamplingRate: probability,
				},
			}
			idx++
		}
		strategy := p.generateDefaultSamplingStrategyResponse()
		strategy.OperationSampling.PerOperationStrategies = opStrategies
		strategies[svc] = &strategy
	}
	p.RUnlock()

	p.Lock()
	defer p.Unlock()
	p.strategyResponses = strategies
}

func (p *processor) generateDefaultSamplingStrategyResponse() sampling.SamplingStrategyResponse {
	return sampling.SamplingStrategyResponse{
		StrategyType: sampling.SamplingStrategyType_PROBABILISTIC,
		OperationSampling: &sampling.PerOperationSamplingStrategies{
			DefaultSamplingProbability:       p.DefaultSamplingProbability,
			DefaultLowerBoundTracesPerSecond: p.LowerBoundTracesPerSecond,
		},
	}
}

func floatEquals(a, b float64) bool {
	return math.Abs(a-b) < 0.0000000001
}