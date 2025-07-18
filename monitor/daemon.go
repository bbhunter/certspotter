// Copyright (C) 2023 Opsmate, Inc.
//
// This Source Code Form is subject to the terms of the Mozilla
// Public License, v. 2.0. If a copy of the MPL was not distributed
// with this file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// This software is distributed WITHOUT A WARRANTY OF ANY KIND.
// See the Mozilla Public License for details.

package monitor

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sync/errgroup"
	"log"
	insecurerand "math/rand"
	"software.sslmate.com/src/certspotter/loglist"
	"time"
)

const (
	reloadLogListIntervalMin = 30 * time.Minute
	reloadLogListIntervalMax = 90 * time.Minute
)

func randomDuration(min, max time.Duration) time.Duration {
	return min + time.Duration(insecurerand.Int63n(int64(max-min+1)))
}

func reloadLogListInterval() time.Duration {
	return randomDuration(reloadLogListIntervalMin, reloadLogListIntervalMax)
}

type task struct {
	log  *loglist.Log
	stop context.CancelFunc
}

type daemon struct {
	config         *Config
	taskgroup      *errgroup.Group
	tasks          map[LogID]task
	logsLoadedAt   time.Time
	logListToken   *loglist.ModificationToken
}

func (daemon *daemon) healthCheck(ctx context.Context) error {
	if time.Since(daemon.logsLoadedAt) >= daemon.config.HealthCheckInterval {
		errors, err := daemon.config.State.GetErrors(ctx, nil, recentErrorCount)
		if err != nil {
			return fmt.Errorf("error getting recent errors: %w", err)
		}
		var errorsDir string
		if fsstate, ok := daemon.config.State.(*FilesystemState); ok {
			errorsDir = fsstate.errorDir(nil)
		}
		info := &StaleLogListInfo{
			Source:        daemon.config.LogListSource,
			LastSuccess:   daemon.logsLoadedAt,
			RecentErrors:  errors,
			ErrorsDir:     errorsDir,
		}
		if err := daemon.config.State.NotifyHealthCheckFailure(ctx, nil, info); err != nil {
			return fmt.Errorf("error notifying about stale log list: %w", err)
		}
	}

	for _, task := range daemon.tasks {
		if err := healthCheckLog(ctx, daemon.config, task.log); err != nil {
			return fmt.Errorf("error checking health of log %q: %w", task.log.GetMonitoringURL(), err)
		}
	}
	return nil
}

func (daemon *daemon) startTask(ctx context.Context, ctlog *loglist.Log) task {
	ctx, cancel := context.WithCancel(ctx)
	daemon.taskgroup.Go(func() error {
		defer cancel()
		err := monitorLogContinously(ctx, daemon.config, ctlog)
		if daemon.config.Verbose {
			log.Printf("%s: task stopped with error: %s", ctlog.GetMonitoringURL(), err)
		}
		if ctx.Err() == context.Canceled && errors.Is(err, context.Canceled) {
			return nil
		} else {
			return fmt.Errorf("error while monitoring %s: %w", ctlog.GetMonitoringURL(), err)
		}
	})
	return task{log: ctlog, stop: cancel}
}

func (daemon *daemon) loadLogList(ctx context.Context) error {
	newLogList, newToken, err := getLogList(ctx, daemon.config.LogListSource, daemon.logListToken)
	if errors.Is(err, loglist.ErrNotModified) {
		return nil
	} else if err != nil {
		return err
	}

	if daemon.config.Verbose {
		log.Printf("fetched %d logs from %q", len(newLogList), daemon.config.LogListSource)
	}

	for logID, task := range daemon.tasks {
		if _, exists := newLogList[logID]; exists {
			continue
		}
		if daemon.config.Verbose {
			log.Printf("stopping task for log %s", logID.Base64String())
		}
		task.stop()
		delete(daemon.tasks, logID)
	}
	for logID, ctlog := range newLogList {
		if _, isRunning := daemon.tasks[logID]; isRunning {
			continue
		}
		if daemon.config.Verbose {
			log.Printf("starting task for log %s (%s)", logID.Base64String(), ctlog.GetMonitoringURL())
		}
		daemon.tasks[logID] = daemon.startTask(ctx, ctlog)
	}
	daemon.logsLoadedAt = time.Now()
	daemon.logListToken = newToken
	return nil
}

func (daemon *daemon) run(ctx context.Context) error {
	if err := daemon.config.State.Prepare(ctx); err != nil {
		return fmt.Errorf("error preparing state: %w", err)
	}

	if err := daemon.loadLogList(ctx); err != nil {
		return fmt.Errorf("error loading log list: %w", err)
	}

	reloadLogListTicker := time.NewTicker(reloadLogListInterval())
	defer reloadLogListTicker.Stop()

	healthCheckTicker := time.NewTicker(daemon.config.HealthCheckInterval)
	defer healthCheckTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-reloadLogListTicker.C:
			if err := daemon.loadLogList(ctx); err != nil {
				recordError(ctx, daemon.config, nil, fmt.Errorf("error reloading log list (will try again later): %w", err))
			}
			reloadLogListTicker.Reset(reloadLogListInterval())
		case <-healthCheckTicker.C:
			if err := daemon.healthCheck(ctx); err != nil {
				return err
			}
		}
	}
}

func Run(ctx context.Context, config *Config) error {
	group, ctx := errgroup.WithContext(ctx)
	daemon := &daemon{
		config:    config,
		taskgroup: group,
		tasks:     make(map[LogID]task),
	}
	group.Go(func() error { return daemon.run(ctx) })
	return group.Wait()
}
