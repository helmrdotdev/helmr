//go:build !linux

package bootgate

import "errors"

func Run() error { return errors.New("bootstrap gate requires Linux") }
