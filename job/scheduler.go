package job

import (
	"sync"
	"time"

	"github.com/golang/glog"
)

type Scheduler struct {
	Name      string
	StartTime time.Duration
	ExecFunc  func()
	Quit      chan bool
	Mutex     sync.Mutex
}

func NewScheduler(deploymentName string, startTime time.Duration, fn func()) (*Scheduler, error) {
	scheduler := &Scheduler{
		Name:      deploymentName,
		StartTime: startTime,
		ExecFunc:  fn,
		Quit:      make(chan bool),
	}

	go func() {
		scheduler.start()
	}()

	return scheduler, nil
}

func (scheduler *Scheduler) start() {
	scheduler.Quit = make(chan bool)
	c := make(chan bool, 1)
	go func() {
		for {
			select {
			case <-c:
				scheduler.ExecFunc()
				return
			case <-scheduler.Quit:
				return
			default:
				time.Sleep(time.Second * 1)
			}
		}
	}()

	select {
	case <-time.After(scheduler.StartTime):
		glog.Infof("Run %s scheduler", scheduler.Name)
		c <- true
	}
}

func (scheduler *Scheduler) Reset(newTime time.Duration) {
	scheduler.Mutex.Lock()
	defer scheduler.Mutex.Unlock()

	scheduler.StartTime = newTime
	scheduler.Quit <- true

	go func() {
		scheduler.start()
	}()
}

func (scheduler *Scheduler) Stop() {
	scheduler.Mutex.Lock()
	defer scheduler.Mutex.Unlock()

	glog.Infof("Stoping %s scheduler", scheduler.Name)
	scheduler.Quit <- true
}
