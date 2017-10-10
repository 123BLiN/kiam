// Copyright 2017 uSwitch
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package metadata

import (
	"fmt"
	"github.com/rcrowley/go-metrics"
	log "github.com/sirupsen/logrus"
	khttp "github.com/uswitch/kiam/pkg/http"
	"github.com/vmg/backoff"
	"net/http"
	"time"
)

var (
	PodNotFound = fmt.Errorf("pod not found")
)

type asyncObj struct {
	obj interface{}
	err error
}

func (s *Server) roleNameHandler(w http.ResponseWriter, req *http.Request) (int, error) {
	requestLog := log.WithFields(khttp.RequestFields(req))
	roleNameTimings := metrics.GetOrRegisterTimer("roleNameHandler", metrics.DefaultRegistry)
	startTime := time.Now()
	defer roleNameTimings.UpdateSince(startTime)

	ip, err := s.clientIP(req)
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("error parsing ip: %s", err.Error())
	}

	respCh := make(chan *asyncObj)
	go func() {
		roleCh := make(chan string, 1)
		op := func() error {
			role, err := s.finder.FindRoleFromIP(ip)
			if err != nil {
				return err
			}

			if role == "" {
				requestLog.Warnf("no pod found for ip")
				return PodNotFound
			}

			roleCh <- role
			return nil
		}

		strategy := backoff.NewExponentialBackOff()
		strategy.MaxElapsedTime = s.cfg.MaxElapsedTime

		err = backoff.Retry(op, backoff.WithContext(strategy, req.Context()))
		if err != nil {
			respCh <- &asyncObj{obj: nil, err: err}
		} else {
			role := <-roleCh
			respCh <- &asyncObj{obj: role, err: nil}
		}
	}()

	select {
	case <-req.Context().Done():
		if req.Context().Err() != nil {
			return http.StatusInternalServerError, req.Context().Err()
		}
	case resp := <-respCh:
		if resp.err == PodNotFound {
			metrics.GetOrRegisterMeter("roleNameHandler.podNotFound", metrics.DefaultRegistry).Mark(1)
			return http.StatusNotFound, fmt.Errorf("pod not found for ip %s", ip)
		} else if resp.err != nil {
			return http.StatusInternalServerError, fmt.Errorf("error finding pod: %s", err.Error())
		}

		role := resp.obj.(string)
		if role == "" {
			return http.StatusNotFound, fmt.Errorf("no role for pod %s", ip)
		}

		fmt.Fprint(w, role)
		metrics.GetOrRegisterMeter("roleNameHandler.success", metrics.DefaultRegistry).Mark(1)
		return http.StatusOK, nil
	}

	return http.StatusOK, nil
}
