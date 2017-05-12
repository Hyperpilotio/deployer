package job

import (
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/spf13/viper"
)

type Scheduler struct {
	Name      string
	StartTime time.Duration
	ExecFunc  func()
	Quit      chan bool
	Mutex     sync.Mutex
}

func NewScheduler(config *viper.Viper, deploymentName string, custShutDownTime string, fn func()) (*Scheduler, error) {
	scheduleRunTime := ""
	if custShutDownTime != "" {
		scheduleRunTime = custShutDownTime
	} else {
		scheduleRunTime = config.GetString("shutDownTime")
	}

	startTime, err := time.ParseDuration(scheduleRunTime)
	if err != nil {
		return nil, fmt.Errorf("Unable to parse shutDownTime %s: %s", scheduleRunTime, err.Error())
	}
	glog.Infof("New %s schedule at %s", deploymentName, time.Now().Add(startTime))

	scheduler := &Scheduler{
		Name:      deploymentName,
		StartTime: startTime,
		ExecFunc:  fn,
		Quit:      make(chan bool),
	}
	scheduler.start()
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

func (scheduler *Scheduler) ResetTimer(newTime time.Duration) {
	scheduler.Mutex.Lock()
	defer scheduler.Mutex.Unlock()

	scheduler.StartTime = newTime
	scheduler.Quit <- true
	scheduler.start()
}

func (scheduler *Scheduler) Stop() {
	scheduler.Mutex.Lock()
	defer scheduler.Mutex.Unlock()

	glog.Infof("Stoping %s scheduler", scheduler.Name)
	scheduler.Quit <- true
}
