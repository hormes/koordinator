/*
Copyright 2022 The Koordinator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package prediction

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"go.uber.org/atomic"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/koordinator-sh/koordinator/pkg/koordlet/metriccache"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/statesinformer"
	"github.com/koordinator-sh/koordinator/pkg/util/histogram"
)

var (
	// MinSampleWeight is the minimal weight of any sample (prior to including decaying factor)
	MinSampleWeight = 0.1
	// epsilon is the minimal weight kept in histograms, it should be small enough that old samples
	// (just inside MemoryAggregationWindowLength) added with MinSampleWeight are still kept
	epsilon = 0.001 * MinSampleWeight
	// DefaultHistogramBucketSizeGrowth is the default value for histogramBucketSizeGrowth.
	DefaultHistogramBucketSizeGrowth = 0.05
	// DefaultMemoryHistogramDecayHalfLife is the default value for MemoryHistogramDecayHalfLife.
	DefaultMemoryHistogramDecayHalfLife = time.Hour * 24
	// DefaultCPUHistogramDecayHalfLife is the default value for CPUHistogramDecayHalfLife.
	DefaultCPUHistogramDecayHalfLife = time.Hour * 12
	// DefaultTrainingInterval is the default interval at which the model runs.
	DefaultTrainingInterval = time.Minute
	// DefaultModelExpirationTime is the default value for model expiration. Models that
	// are not updated after this time will be GC.
	DefaultModelExpirationTime = time.Minute * 30
	// DefaultModelCheckPointInterval is the default value for model checkpoint.
	DefaultModelCheckpointInterval   = time.Minute * 10
	DefaultModelCheckpointMaxPerStep = 12
)

/*
PredictServer is responsible for fetching data from MetricCache, training prediction results
according to predefined models, and providing an interface for obtaining prediction results.

It is important to note that the prediction results made by PredictServer based on the captured
data are only related to the data it sees. For example, when we need to deal with cold starts,
this business logic should be processed when using the predicted data instead of being coupled
to the predictive model.

The predictive model currently provides histogram-based statistics with exponentially decaying
weights over time periods. PredictServer is responsible for storing the intermediate results of
the model and recovering when the process restarts.
*/
type PredictServer interface {
	Run(stopCh <-chan struct{}) error
	HasSynced() bool
	GetPrediction(MetricDesc) (Result, error)
}

type PredictModel struct {
	CPU    histogram.Histogram
	Memory histogram.Histogram

	LastUpdated      time.Time
	LastCheckpointed time.Time
	Lock             sync.Mutex
}

type peakPredictServer struct {
	informer     Informer
	metricServer MetricServer

	uidGenerator UIDGenerator
	models       map[UIDType]*PredictModel
	modelsLock   sync.Mutex

	clock        clock.Clock
	hasSynced    *atomic.Bool
	checkpointer Checkpointer
}

func NewPeakPredictServer(statesInformer statesinformer.StatesInformer, metricCache metriccache.MetricCache, options Options) PredictServer {
	return &peakPredictServer{
		informer:     NewInformer(statesInformer),
		metricServer: NewMetricServer(metricCache),

		uidGenerator: &generator{},
		models:       make(map[UIDType]*PredictModel),
		clock:        clock.RealClock{},
		hasSynced:    &atomic.Bool{},
		checkpointer: NewFileCheckpointer(options.Filepath),
	}
}

func (p *peakPredictServer) Run(stopCh <-chan struct{}) error {
	if !cache.WaitForCacheSync(stopCh, p.informer.HasSynced) {
		return fmt.Errorf("time out waiting for states informer caches to sync")
	}

	unknownUIDs := p.restoreModels()

	// remove unknown checkpoints before starting to work
	for _, uid := range unknownUIDs {
		err := p.checkpointer.Remove(uid)
		klog.InfoS("remove unknown checkpoint", "uid", uid)
		if err != nil {
			klog.Errorf("remove checkpoint %v failed, err: %v", uid, err)
		}
	}

	go wait.Until(p.training, DefaultTrainingInterval, stopCh)
	go wait.Until(p.gcModels, time.Minute, stopCh)
	go wait.Until(p.doCheckpoint, time.Minute, stopCh)
	<-stopCh
	return nil
}

func (p *peakPredictServer) HasSynced() bool {
	return p.hasSynced.Load()
}

func (p *peakPredictServer) training() {
	// get pod metrics
	// 1. list pods, update models
	pods := p.informer.ListPods()
	for _, pod := range pods {
		uid := p.uidGenerator.Pod(pod)
		lastCPUUsage, err := p.metricServer.GetPodMetric(MetricDesc{UID: uid}, CPUUsage)
		if err != nil {
			klog.Warning("failed to query pod cpu metric", err)
			continue
		}
		lastMemoryUsage, err := p.metricServer.GetPodMetric(MetricDesc{UID: uid}, MemoryUsage)
		if err != nil {
			klog.Warning("failed to query pod memory metric", err)
			continue
		}

		// update the model
		p.updateMode(uid, lastCPUUsage, lastMemoryUsage)
	}

	// 2. get node, update models
	node := p.informer.GetNode()
	nodeUID := p.uidGenerator.Node(node)
	lastNodeCPUUsage, errCPU := p.metricServer.GetNodeMetric(MetricDesc{UID: nodeUID}, CPUUsage)
	lastNodeMemoryUsage, errMem := p.metricServer.GetNodeMetric(MetricDesc{UID: nodeUID}, MemoryUsage)
	if errCPU != nil || errMem != nil {
		klog.Warning("failed to query node cpu and memory metric", errCPU, errMem)
	} else {
		p.updateMode(nodeUID, lastNodeCPUUsage, lastNodeMemoryUsage)
	}

	p.hasSynced.Store(true)
}

// From 0.05 to 1024 cores, maintain the bucket of the CPU histogram at a rate of 5%
func defaultCPUHistogram() histogram.Histogram {
	options, err := histogram.NewExponentialHistogramOptions(1024, 0.025, 1.+DefaultHistogramBucketSizeGrowth, epsilon)
	if err != nil {
		klog.Fatal("failed to create CPU HistogramOptions")
	}
	return histogram.NewDecayingHistogram(options, DefaultCPUHistogramDecayHalfLife)
}

// From 10M to 2T, maintain the bucket of the Memory histogram at a rate of 5%
func defaultMemoryHistogram() histogram.Histogram {
	options, err := histogram.NewExponentialHistogramOptions(1<<31, 5<<20, 1.+DefaultHistogramBucketSizeGrowth, epsilon)
	if err != nil {
		klog.Fatal("failed to create Memory HistogramOptions")
	}
	return histogram.NewDecayingHistogram(options, DefaultMemoryHistogramDecayHalfLife)
}

func (p *peakPredictServer) updateMode(uid UIDType, cpu, memory float64) {
	p.modelsLock.Lock()
	defer p.modelsLock.Unlock()
	model, ok := p.models[uid]
	if !ok {
		model = &PredictModel{
			CPU:    defaultCPUHistogram(),
			Memory: defaultMemoryHistogram(),
		}
		p.models[uid] = model
	}
	now := p.clock.Now()
	model.Lock.Lock()
	defer model.Lock.Unlock()
	model.LastUpdated = now
	// TODO Add adjusted weights
	model.CPU.AddSample(cpu, 1, now)
	model.Memory.AddSample(memory, 1, now)
}

func (p *peakPredictServer) GetPrediction(metric MetricDesc) (Result, error) {
	p.modelsLock.Lock()
	defer p.modelsLock.Unlock()
	model, ok := p.models[metric.UID]
	if !ok {
		return Result{}, fmt.Errorf("UID %v node found in predict server", metric.UID)
	}
	model.Lock.Lock()
	defer model.Lock.Unlock()
	//
	return Result{
		Data: map[string]v1.ResourceList{
			"p60": {
				v1.ResourceCPU:    *resource.NewMilliQuantity(int64(model.CPU.Percentile(0.6)*1000.0), resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(int64(model.Memory.Percentile(0.6)), resource.BinarySI),
			},
			"p90": {
				v1.ResourceCPU:    *resource.NewMilliQuantity(int64(model.CPU.Percentile(0.9)*1000.0), resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(int64(model.Memory.Percentile(0.9)), resource.BinarySI),
			},
			"p95": {
				v1.ResourceCPU:    *resource.NewMilliQuantity(int64(model.CPU.Percentile(0.95)*1000.0), resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(int64(model.Memory.Percentile(0.95)), resource.BinarySI),
			},
			"p98": {
				v1.ResourceCPU:    *resource.NewMilliQuantity(int64(model.CPU.Percentile(0.98)*1000.0), resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(int64(model.Memory.Percentile(0.98)), resource.BinarySI),
			},
			"max": {
				v1.ResourceCPU:    *resource.NewMilliQuantity(int64(model.CPU.Percentile(1.0)*1000.0), resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(int64(model.Memory.Percentile(1.0)), resource.BinarySI),
			},
		},
	}, nil
}

func (p *peakPredictServer) gcModels() {
	if !p.HasSynced() {
		klog.Infof("wait for the state to be synchronized, skipping the step of model GC")
		return
	}

	tobeRemovedModels := make([]UIDType, 0)
	p.modelsLock.Lock()
	for uid, model := range p.models {
		if p.clock.Since(model.LastUpdated) > DefaultModelExpirationTime {
			delete(p.models, uid)
			klog.InfoS("gc model", "uid", uid)
			tobeRemovedModels = append(tobeRemovedModels, uid)
		}
	}
	p.modelsLock.Unlock()

	// do the io operations out of lock
	for _, uid := range tobeRemovedModels {
		err := p.checkpointer.Remove(uid)
		klog.InfoS("remove checkpoint", "uid", uid)
		if err != nil {
			klog.Errorf("remove checkpoint %v failed, err: %v", uid, err)
		}
	}
}

func (p *peakPredictServer) doCheckpoint() {
	if !p.HasSynced() {
		klog.Infof("wait for the state to be synchronized, skipping the step of model GC")
		return
	}

	type pair struct {
		UID   UIDType
		Model *PredictModel
	}

	p.modelsLock.Lock()
	pairs := make([]pair, 0, len(p.models))
	for key, model := range p.models {
		pairs = append(pairs, pair{UID: key, Model: model})
	}
	p.modelsLock.Unlock()

	// Sort models and keys by LastCheckpointed time
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].Model.LastCheckpointed.Before(pairs[j].Model.LastCheckpointed)
	})

	checkpointModelsCount := 0
	for _, pair := range pairs {
		if checkpointModelsCount >= DefaultModelCheckpointMaxPerStep {
			break
		}
		if p.clock.Since(pair.Model.LastCheckpointed) < DefaultModelCheckpointInterval {
			break
		}
		ckpt := ModelCheckpoint{
			UID:         pair.UID,
			LastUpdated: metav1.NewTime(p.clock.Now()),
		}
		pair.Model.Lock.Lock()
		ckpt.CPU, _ = pair.Model.CPU.SaveToCheckpoint()
		ckpt.Memory, _ = pair.Model.Memory.SaveToCheckpoint()
		pair.Model.Lock.Unlock()

		err := p.checkpointer.Save(ckpt)
		if err != nil {
			klog.Errorf("save checkpoint uid %v failed", pair.UID)
		} else {
			klog.InfoS("save checkpoint", "uid", pair.UID)
		}
		pair.Model.LastCheckpointed = p.clock.Now()
		checkpointModelsCount++
	}
}

func (p *peakPredictServer) restoreModels() (unknownUIDs []UIDType) {
	checkpoints, err := p.checkpointer.Restore()
	if err != nil {
		klog.Errorf("restore models failed, err %v", err)
		return
	}

	knownUIDs := make(map[UIDType]bool)
	pods := p.informer.ListPods()
	for _, pod := range pods {
		knownUIDs[UIDType(pod.UID)] = true
	}
	node := p.informer.GetNode()
	if node != nil {
		knownUIDs[UIDType(node.UID)] = true
	}

	for _, checkpoint := range checkpoints {
		if checkpoint.Error != nil || !knownUIDs[checkpoint.UID] {
			unknownUIDs = append(unknownUIDs, checkpoint.UID)
			continue
		}

		model := &PredictModel{
			CPU:         defaultCPUHistogram(),
			Memory:      defaultMemoryHistogram(),
			LastUpdated: checkpoint.LastUpdated.Time,
		}
		if err := model.CPU.LoadFromCheckpoint(checkpoint.CPU); err != nil {
			klog.Errorf("failed to CPU checkpoint %v, err %v", checkpoint.UID, err)
		}
		if err := model.Memory.LoadFromCheckpoint(checkpoint.Memory); err != nil {
			klog.Errorf("failed to Memory checkpoint %v, err %v", checkpoint.UID, err)
		}
		klog.InfoS("restoring checkpoint", "uid", checkpoint.UID, "lastUpdated", checkpoint.LastUpdated)
		p.modelsLock.Lock()
		p.models[checkpoint.UID] = model
		p.modelsLock.Unlock()
	}

	return unknownUIDs
}
