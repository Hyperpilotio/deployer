package job

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

type Scheduler struct {
	CreateTime time.Time
	StartTime  time.Duration
	NewTime    time.Duration
	ExecFunc   func()
	Reset      chan bool
	Quit       chan bool
}

func NewScheduler(config *viper.Viper) (*Scheduler, error) {
	shutDownTime, err := time.ParseDuration(config.GetString("shutDownTime"))
	if err != nil {
		return nil, fmt.Errorf("Unable to parse shutDownTime %s: %s", shutDownTime, err.Error())
	}

	return &Scheduler{
		CreateTime: time.Now(),
		StartTime:  shutDownTime,
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
				scheduler.ExecFunc()
				timer.Stop()
				return
			case <-scheduler.Reset:
				timer.Reset(scheduler.NewTime)
			case <-scheduler.Quit:
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
