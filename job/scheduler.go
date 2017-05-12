package job

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/spf13/viper"
)

type Scheduler struct {
	Name       string
	CreateTime time.Time
	StartTime  time.Duration
	NewTime    time.Duration
	ExecFunc   func()
	Reset      chan bool
	Quit       chan bool
}

func NewScheduler(config *viper.Viper, deploymentName string, custShutDownTime string) (*Scheduler, error) {
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

	return &Scheduler{
		Name:       deploymentName,
		CreateTime: time.Now(),
		StartTime:  startTime,
		Reset:      make(chan bool),
		Quit:       make(chan bool),
	}, nil
}

func (scheduler *Scheduler) AddFunc(fn func()) {
	scheduler.ExecFunc = fn
}

func (scheduler *Scheduler) Run() {
	timer := time.NewTimer(scheduler.StartTime)
	go func() {
		for {
			select {
			case <-timer.C:
				glog.Infof("Run %s scheduler", scheduler.Name)
				scheduler.ExecFunc()
				timer.Stop()
				return
			case <-scheduler.Reset:
				timer.Reset(scheduler.NewTime)
			case <-scheduler.Quit:
				glog.Infof("Stoping %s scheduler", scheduler.Name)
				return
			}
		}
	}()
}

func (scheduler *Scheduler) ResetTimer(newTime time.Duration) {
	scheduler.NewTime = newTime
	scheduler.Reset <- true
}

func (scheduler *Scheduler) Stop() {
	scheduler.Quit <- true
}
