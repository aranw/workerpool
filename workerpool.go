/*
Package workerpool queues work to a limited number of goroutines.

The purpose of the worker pool is to limit the concurrency of the task
performed by the workers.  This is useful when performing a task requires
sufficient resources (CPU, memory, etc.), that running too many tasks at the
same time would exhaust resources.

Non-blocking task submission

A task is a function submitted to the worker pool for execution.  Submitting
tasks to this worker pool will not block, regardless of the number of tasks.
Tasks read from the input task queue are immediately dispatched to an available
worker.  If no worker is immediately available, then the task is passed to a go
routine which is started to wait for an available worker.  This clears the task
from the task queue immediately, whether or not a worker is currently
available, and will not block the submission of tasks.

The intent of the worker pool is to limit the concurrency of task execution,
not limit the number of tasks queued to be executed. Therefore, this unbounded
input of tasks is acceptable as the tasks cannot be discarded.  If the number
of inbound tasks if too many to even queue for pending processing, then this
solution is outside the scope of the worker pool, and should be solved by
distributing load over multiple systems, storing input that requires processing
in some intermediate storage (e.g. a database or file system).

Dispatcher

This worker pool uses a single dispatcher goroutine to read tasks from the
input task queue and dispatch them to a worker goroutine.  This allows for a
small input channel, and lets the dispatcher queue as many tasks as are
submitted when there are no available workers (using goroutines).
Additionally, the dispatcher can adjust the number of workers as appropriate
for the work load, without having to utilize locked counters and checks
incurred on task submission.

Usage note

It is advisable to use different worker pools for tasks that are bound by
different resources, or that have different resource use patterns.  For
example, tasks that use X Mb of memory may need different concurrency limits
than tasks that use Y Mb of memory.

Credits

This implementation builds on ideas from the following:

http://marcio.io/2015/07/handling-1-million-requests-per-minute-with-golang
http://nesv.github.io/golang/2014/02/25/worker-queues-in-go.html

*/
package workerpool

import "time"

const (
	// Size of queue to which tasks are submitted.  This can be small, no
	// matter how many tasks are submitted, because the dispatcher removes
	// tasks from this queue, scheduling each immediately to a ready worker, or
	// to a goroutine that will give it to the next ready worker.
	taskQueueSize = 16

	// If worker pool receives no new work for this period of time, then stop
	// a worker goroutine.
	idleTimeoutSec = 5

	// The minimum number of workers is always zero, because the time to start
	// new workers is insignificant.
	minWorkers = 0
)

type WorkerPool interface {
	// Submit enqueues a function for a worker to execute.
	//
	// Any external values needed by the task function must be captured in a
	// closure.  Any return values should be returned over a channel that is
	// captured in the task function closure.
	//
	// Submit will not block regardless of the number of tasks submitted.  Each
	// task is immediately given to an available worker or passed to a
	// goroutine to be given to the next available worker.  If there are no
	// available workers, the dispatcher adds a worker, until the maximum
	// number of workers is running.
	Submit(task func())

	// Stop stops the worker pool and waits for workers to complete.
	//
	// Since creating the worker pool starts at least one goroutine, for the
	// dispatcher, this function should be called when the worker pool is no
	// longer needed.
	Stop()

	// Stopped returns true if this worker pool has been stopped.
	Stopped() bool

	// WorkerCount returns the current number of worker goroutines.
	//
	// Do not rely on this value to be perfectly accurate as it may change by
	// the time the caller gets it.
	WorkerCount() int
}

// New creates and starts a pool of worker goroutines.
//
// The maxWorkers parameter specifies the maximum number of workers that will
// execute tasks concurrently.  After each timeout period, a worker goroutine
// is stopped until there are no remaining workers.
func NewWP(maxWorkers int) (WorkerPool, error) {
	// There must be at least one worker.
	if maxWorkers < 1 {
		maxWorkers = 1
	}

	pool := &workerPool{
		taskQueue:    make(chan func(), taskQueueSize),
		maxWorkers:   maxWorkers,
		readyWorkers: make(chan chan func(), maxWorkers),
		timeout:      time.Second * idleTimeoutSec,
		stoppedChan:  make(chan struct{}),
	}

	// Start the task dispatcher.
	go pool.dispatch()

	return pool, nil
}

type workerPool struct {
	maxWorkers   int
	workerCount  int
	timeout      time.Duration
	taskQueue    chan func()
	readyWorkers chan chan func()
	stoppedChan  chan struct{}
}

// Stop stops the worker pool and waits for workers to complete.
func (p *workerPool) Stop() {
	if p.Stopped() {
		return
	}
	close(p.taskQueue)
	<-p.stoppedChan
}

// Stopped returns true if this worker pool has been stopped.
func (p *workerPool) Stopped() bool {
	select {
	case <-p.stoppedChan:
		return true
	default:
	}
	return false
}

// Submit enqueues a function for a worker to execute.
func (p *workerPool) Submit(task func()) {
	if task != nil {
		p.taskQueue <- task
	}
}

// WorkerCount returns the current number of worker goroutines.
func (p *workerPool) WorkerCount() int { return p.workerCount }

// dispatch sends the next queued task to an available worker.
func (p *workerPool) dispatch() {
	defer close(p.stoppedChan)
	timeout := time.NewTimer(p.timeout)
	var task func()
	var ok bool
	var workerTaskChan chan func()
shutdown:
	for {
		timeout.Reset(p.timeout)
		select {
		case task, ok = <-p.taskQueue:
			if !ok {
				break shutdown
			}
			// Got a task to do.
			select {
			case workerTaskChan = <-p.readyWorkers:
				// A worker is ready, so give task to worker.
				workerTaskChan <- task
			default:
				// No workers ready.
				// Create a new worker, if not at max.
				if p.workerCount < p.maxWorkers {
					p.workerCount++
					startWorker(p.readyWorkers)
				}
				// Start a goroutine to submit the task when a worker is ready.
				go func(t func()) {
					taskChan := <-p.readyWorkers
					taskChan <- t
				}(task)
			}
		case <-timeout.C:
			// Timed out waiting for work to arrive.  Kill a ready worker.
			if p.workerCount > minWorkers {
				select {
				case workerTaskChan = <-p.readyWorkers:
					// A worker is ready, so kill.
					close(workerTaskChan)
					p.workerCount--
				default:
					// No work, but no ready workers.  All workers are busy.
				}
			}
		}
	}

	// Stop all remaining workers as they become ready.
	for p.workerCount > 0 {
		workerTaskChan = <-p.readyWorkers
		close(workerTaskChan)
		p.workerCount--
	}
}

// startWorker starts a goroutine that executes tasks given by the dispatcher.
//
// A worker registers that is it available to do work by putting its task
// channel on the readyWorkers channel.  The dispatcher reads a worker's task
// channel from the readyWorkers channel, and writes a task to the worker over
// the worker's task channel.  To stop a worker, the dispatcher closes a
// worker's task channel, instead of writing a task to it.
func startWorker(readyWorkers chan chan func()) {
	go func() {
		taskChan := make(chan func())
		var task func()
		var ok bool
		for {
			// Register availability on readyWorkers channel.
			readyWorkers <- taskChan

			// Read task from dispatcher.
			task, ok = <-taskChan
			if !ok {
				// Dispatcher has told worker to stop.
				break
			}

			// Execute the task.
			task()
		}
	}()
}
