package server

import (
	"fmt"
)

var (
	// ErrPodNotFound returned when no pod found with a matching IP
	ErrPodNotFound = fmt.Errorf("no pod found")
	// ErrPolicyForbidden returned when credentials can't be issued
	// because of a policy
	ErrPolicyForbidden = fmt.Errorf("forbidden by policy")
)
