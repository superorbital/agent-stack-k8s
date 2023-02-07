package scheduler

import (
	"context"
	"fmt"
	"sync"

	"github.com/buildkite/agent-stack-k8s/api"
	"github.com/buildkite/agent-stack-k8s/monitor"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type MaxInFlightLimiter struct {
	scheduler   monitor.JobHandler
	MaxInFlight int

	logger      *zap.Logger
	mu          sync.RWMutex
	inFlight    map[string]struct{}
	completions chan struct{}
}

func NewLimiter(logger *zap.Logger, scheduler monitor.JobHandler, maxInFlight int) *MaxInFlightLimiter {
	return &MaxInFlightLimiter{
		scheduler:   scheduler,
		MaxInFlight: maxInFlight,
		logger:      logger,
		inFlight:    make(map[string]struct{}),
		completions: make(chan struct{}, maxInFlight),
	}
}

// Creates a Jobs informer, registers the handler on it, and waits for cache sync
func RegisterInformer(ctx context.Context, clientset kubernetes.Interface, tags []string, handler cache.ResourceEventHandler) error {
	hasTag, err := labels.NewRequirement(api.TagLabel, selection.In, api.TagsToLabels(tags))
	if err != nil {
		return fmt.Errorf("failed to build tag label selector for job manager: %w", err)
	}
	hasUUID, err := labels.NewRequirement(api.UUIDLabel, selection.Exists, nil)
	if err != nil {
		return fmt.Errorf("failed to build uuid label selector for job manager: %w", err)
	}
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0, informers.WithTweakListOptions(func(opt *metav1.ListOptions) {
		opt.LabelSelector = labels.NewSelector().Add(*hasTag, *hasUUID).String()
	}))
	informer := factory.Batch().V1().Jobs()
	jobInformer := informer.Informer()
	if _, err := jobInformer.AddEventHandler(handler); err != nil {
		return fmt.Errorf("failed to register event handler: %w", err)
	}

	go factory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), jobInformer.HasSynced) {
		return fmt.Errorf("failed to sync informer cache")
	}

	return nil
}

func (l *MaxInFlightLimiter) Create(ctx context.Context, job *monitor.Job) error {
	l.mu.RLock()
	inFlight := len(l.inFlight)
	l.mu.RUnlock()
	if l.MaxInFlight > 0 && inFlight >= l.MaxInFlight {
		l.logger.Debug("max-in-flight reached", zap.Int("in-flight", inFlight))
		<-l.completions // wait for a completion
	}

	select {
	case <-ctx.Done():
		return nil
	default:
		return l.add(ctx, job)
	}
}

func (l *MaxInFlightLimiter) add(ctx context.Context, job *monitor.Job) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, found := l.inFlight[job.Uuid]; found {
		l.logger.Debug("skipping already queued job", zap.String("uuid", job.Uuid))
		return nil
	}
	if err := l.scheduler.Create(ctx, job); err != nil {
		return err
	}
	l.inFlight[job.Uuid] = struct{}{}
	return nil
}

// load jobs at controller startup/restart
func (l *MaxInFlightLimiter) OnAdd(obj interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	job := obj.(*batchv1.Job)
	if !isFinished(job) {
		uuid := job.Labels[api.UUIDLabel]
		if _, alreadyInFlight := l.inFlight[uuid]; !alreadyInFlight {
			l.logger.Debug("adding in-flight job", zap.String("uuid", uuid), zap.Int("in-flight", len(l.inFlight)))
			l.inFlight[uuid] = struct{}{}
		}
	}
}

// if a job is still running, add it to inFlight, otherwise try to remove it
func (l *MaxInFlightLimiter) OnUpdate(_, obj interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	job := obj.(*batchv1.Job)
	uuid := job.Labels[api.UUIDLabel]
	if isFinished(job) {
		l.markComplete(job)
	} else {
		if _, alreadyInFlight := l.inFlight[uuid]; !alreadyInFlight {
			l.logger.Debug("waiting for job completion", zap.String("uuid", uuid))
			l.inFlight[uuid] = struct{}{}
		}
	}
}

// if jobs are deleted before they complete, ensure we remove them from inFlight
func (l *MaxInFlightLimiter) OnDelete(obj interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.markComplete(obj.(*batchv1.Job))
}

func (l *MaxInFlightLimiter) markComplete(job *batchv1.Job) {
	uuid := job.Labels[api.UUIDLabel]
	if _, alreadyInFlight := l.inFlight[uuid]; alreadyInFlight {
		l.logger.Debug("job complete", zap.String("uuid", uuid), zap.Int("in-flight", len(l.inFlight)))
		delete(l.inFlight, uuid)
		l.completions <- struct{}{}
	}
}

func isFinished(job *batchv1.Job) bool {
	var finished bool
	for _, cond := range job.Status.Conditions {
		switch cond.Type {
		case batchv1.JobComplete, batchv1.JobFailed:
			finished = true
		}
	}
	return finished
}
