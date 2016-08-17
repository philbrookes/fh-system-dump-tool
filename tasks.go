package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
)

// A Task performs some part of the RHMAP System Dump Tool.
type Task func() error

// RunAllTasks runs all tasks known to the dump tool using concurrent workers.
// Dump output goes to path.
func RunAllTasks(runner Runner, path string, workers int) {
	tasks := GetAllTasks(runner, path)
	results := make(chan error)

	// Start worker goroutines to run tasks concurrently.
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for task := range tasks {
				results <- task()
			}
		}()
	}
	// Wait for all workers to terminate, then close the results channel to
	// communicate that no more results will be sent.
	go func() {
		wg.Wait()
		close(results)
	}()
	// Loop through the task execution results and log errors.
	for err := range results {
		if err != nil {
			// TODO: there should be a way to identify which task
			// had an error.
			fmt.Fprintln(os.Stderr)
			log.Printf("Task error: %v", err)
			continue
		}
		fmt.Fprint(os.Stderr, ".")
	}
	fmt.Fprintln(os.Stderr)
}

// GetAllTasks returns a channel of all tasks known to the dump tool. It returns
// immediately and sends tasks to the channel in a separate goroutine. The
// channel is closed after all tasks are sent.
// FIXME: GetAllTasks should not need to know about basepath.
func GetAllTasks(runner Runner, basepath string) <-chan Task {
	var (
		resources = []string{"deploymentconfigs", "pods", "services", "events"}
		// We should only care about logs for pods, because they cover
		// all other possible types.
		resourcesWithLogs = []string{"pods"}
	)
	tasks := make(chan Task)
	go func() {
		defer close(tasks)

		projects, err := GetProjects()
		if err != nil {
			tasks <- NewError(err)
			return
		}
		if len(projects) == 0 {
			tasks <- NewError(errors.New("no projects visible to the currently logged in user"))
			return
		}

		var wg sync.WaitGroup

		// Add tasks to fetch resource definitions.
		wg.Add(1)
		go func() {
			defer wg.Done()
			GetResourceDefinitionsTasks(tasks, projects, resources, basepath)
		}()

		// Add tasks to fetch logs.
		wg.Add(1)
		go func() {
			defer wg.Done()
			GetFetchLogsTasks(tasks, runner, projects, resourcesWithLogs)
		}()

		// Add tasks to fetch Nagios data.
		wg.Add(1)
		go func() {
			defer wg.Done()
			GetNagiosTasks(tasks, projects, basepath, getResourceNamesBySubstr)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			tasks <- GetOcAdmDiagnosticsTask(runner)
		}()

		wg.Wait()

		// After all other tasks are done, add analysis tasks. We want
		// to run them strictly later so that they can leverage the
		// output of commands executed previously by other tasks, e.g.,
		// reading resource definitions.
		for _, p := range projects {
			outFor := outToFile(basepath, "json", "analysis")
			errOutFor := outToFile(basepath, "stderr", "analysis")
			tasks <- CheckTasks(p, outFor, errOutFor)
		}
	}()
	return tasks
}

// NewError returns a Task that always return the given error.
func NewError(err error) Task {
	return func() error { return err }
}

type ResourceMatchFactory func(project, resource, substr string) ([]string, error)

// GetNagiosTasks sends tasks to dump Nagios data for each project that contain
// a Nagios pod. This function will output an error to the user if no Nagios pods
// were found in any projects.
func GetNagiosTasks(tasks chan<- Task, projects []string, basepath string, resourceFactory ResourceMatchFactory) {
	foundANagiosPod := false
	for _, p := range projects {
		pods, err := resourceFactory(p, "pod", "nagios")
		if err != nil {
			tasks <- NewError(err)
			continue
		}
		for _, pod := range pods {
			foundANagiosPod = true
			outFor := outToFile(basepath, "dat", "nagios")
			errOutFor := outToFile(basepath, "stderr", "nagios")
			tasks <- GetNagiosStatusData(p, pod, outFor, errOutFor)

			outFor = outToFile(basepath, "tar", "nagios")
			errOutFor = outToFile(basepath, "stderr", "nagios")
			tasks <- GetNagiosHistoricalData(p, pod, outFor, errOutFor)
		}
	}

	if !foundANagiosPod {
		tasks <- NewError(errors.New("A Nagios pod could not be found in any project. For a more thorough analysis, please ensure Nagios is running in all RHMAP projects."))
	}
}

// GetResourceDefinitionsTasks sends tasks to fetch the definitions of all
// resources in all projects.
// FIXME: GetResourceDefinitionsTasks should not know about basepath.
func GetResourceDefinitionsTasks(tasks chan<- Task, projects, resources []string, basepath string) {
	for _, p := range projects {
		outFor := outToFile(basepath, "json", "definitions")
		errOutFor := outToFile(basepath, "stderr", "definitions")
		tasks <- ResourceDefinitions(p, resources, outFor, errOutFor)
	}
}

// GetFetchLogsTasks sends tasks to fetch current and previous logs of all
// resources in all projects.
func GetFetchLogsTasks(tasks chan<- Task, runner Runner, projects, resources []string) {
	loggableResources, err := GetLogabbleResources(projects, resources)
	if err != nil {
		tasks <- NewError(err)
		// continue and iterate over loggableResources even if there was
		// an error.
	}
	for _, r := range loggableResources {
		// Send task to fetch current logs.
		tasks <- FetchLogs(runner, r, *maxLogLines)
		// Send task to fetch previous logs.
		tasks <- FetchPreviousLogs(runner, r, *maxLogLines)
	}
}

// GetLogabbleResources returns a list of loggable resources. It may return
// results even in the presence of an error.
func GetLogabbleResources(projects, resources []string) ([]LoggableResource, error) {
	var (
		loggableResources []LoggableResource
		errors            errorList
	)
	for _, p := range projects {
		for _, rtype := range resources {
			names, err := GetResourceNames(p, rtype)
			if err != nil {
				errors = append(errors, err)
				continue
			}
			for _, name := range names {
				resources, err := GetLoggableResources(p, rtype, name)
				if err != nil {
					errors = append(errors, err)
					continue
				}
				loggableResources = append(loggableResources, resources...)
			}
		}
	}
	if len(errors) > 0 {
		return loggableResources, errors
	}
	return loggableResources, nil
}
