// Copyright © 2021 Bin Liu <bin.liu@enmotech.com>

package exporter

import (
	"fmt"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"strings"
	"sync"
	"time"
)

// 查询监控指标. 先判断是否读取缓存. 禁用缓存或者缓存超时,则读取数据库
func (s *Server) queryMetrics(ch chan<- prometheus.Metric) map[string]error {
	metricErrors := make(map[string]error)
	wg := sync.WaitGroup{}
	limit := newRateLimit(s.parallel)
	for metric, queryInstance := range s.queryInstanceMap {
		if metric == "pg_primary_only" {
			fmt.Println(metric)
		}
		if !s.primary && queryInstance.Primary {
			log.Infof("Collect Metric %s only run primary. instance is recovery auto skip", metric)
			continue
		}
		wg.Add(1)
		queryInst := queryInstance
		metricName := metric
		limit.getToken()
		go func() {
			defer wg.Done()
			defer limit.putToken()
			err := s.queryMetric(ch, queryInst)
			if err != nil {
				metricErrors[metricName] = err
			}
		}()

	}
	wg.Wait()

	return metricErrors
}

func (s *Server) queryMetric(ch chan<- prometheus.Metric, queryInstance *QueryInstance) error {
	var (
		metric         = queryInstance.Name
		scrapeMetric   = false // Whether to collect indicators from the database 是否从数据库里采集指标
		cachedMetric   = &cachedMetrics{}
		metrics        []prometheus.Metric
		nonFatalErrors []error
		err            error
	)

	// log.Debugf("Querying metric : %s", metric)
	if !s.primary && queryInstance.Primary {
		// log.Infof("Collect Metric %s only run primary. instance is recovery auto skip", metric)
		return nil
	}
	querySQL := queryInstance.GetQuerySQL(s.lastMapVersion)
	if querySQL == nil {
		log.Errorf("Collect Metric %s not define querySQL for version %s", metric, s.lastMapVersion.String())
		return nil
	}
	if strings.EqualFold(querySQL.Status, statusDisable) {
		log.Debugf("Collect Metric %s disable. skip", metric)
		return nil
	}
	// Determine whether to enable caching and cache expiration 判断是否启用缓存和缓存过期
	if !s.disableCache {
		var found bool
		// Check if the metric is cached
		s.cacheMtx.Lock()
		cachedMetric, found = s.metricCache[metric]
		s.cacheMtx.Unlock()
		// If found, check if needs refresh from cache
		if !found {
			scrapeMetric = true
		} else if !cachedMetric.IsValid(queryInstance.TTL) {
			scrapeMetric = true
		}
	} else {
		scrapeMetric = true
	}
	if scrapeMetric {
		metrics, nonFatalErrors, err = s.doCollectMetric(queryInstance)
	} else {
		metrics, nonFatalErrors = cachedMetric.metrics, cachedMetric.nonFatalErrors
	}

	// Serious error - a namespace disappeared
	if err != nil {
		nonFatalErrors = append(nonFatalErrors, err)
		log.Errorf("Collect Metric [%s] err %s", metric, err)
	}
	// Non-serious errors - likely version or parsing problems.
	if len(nonFatalErrors) > 0 {
		var errText string
		for _, err := range nonFatalErrors {
			log.Errorf("Collect Metric [%s] nonFatalErrors err %s", metric, err)
			errText += err.Error()
		}
		err = errors.New(errText)
	}

	// Emit the metrics into the channel
	for _, metric := range metrics {
		ch <- metric
	}

	if scrapeMetric && queryInstance.TTL > 0 {
		// Only cache if metric is meaningfully cacheable
		s.cacheMtx.Lock()
		s.metricCache[metric] = &cachedMetrics{
			metrics:        metrics,
			lastScrape:     time.Now(), // 改为查询完时间
			nonFatalErrors: nonFatalErrors,
		}
		s.cacheMtx.Unlock()
	}
	return err
}