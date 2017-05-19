package job

import (
	"sync"
	"time"

	"github.com/golang/glog"
)

type Scheduler struct {
	timer *time.Timer
	mutex sync.Mutex
}

func NewScheduler(duration time.Duration, f func()) *Scheduler {
	return &Scheduler{
		timer: time.AfterFunc(duration, f),
	}
}
func (scheduler *Scheduler) Reset(duration time.Duration) bool {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()

	if scheduler.timer == nil || !scheduler.timer.Stop() {
		glog.Warningf("Scheduler already fired, skipping reset")
		return false
	}

	return scheduler.timer.Reset(duration)
}

func (scheduler *Scheduler) Stop() {
	scheduler.mutex.Lock()
	defer scheduler.mutex.Unlock()

	if scheduler.timer == nil || !scheduler.timer.Stop() {
		glog.Warningf("Scheduler already fired, skipping reset")
	}

	scheduler.timer = nil
}
