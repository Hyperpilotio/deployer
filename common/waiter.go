package common

import (
	"errors"
	"time"
)

func WaitUntilFunctionComplete(timeout time.Duration, f func() bool) error {
	c := make(chan bool, 1)
	quit := make(chan bool)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				ok := f()
				if ok {
					c <- true
				}
				time.Sleep(time.Second * 10)
			}
		}
	}()

	select {
	case <-c:
		return nil
	case <-time.After(timeout):
		quit <- true
		return errors.New("Timed out waiting for function condition to be true")
	}
}
