package processor

import (
	"archive/zip"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/alfreddobradi/go-bb-man/database"
	"github.com/alfreddobradi/go-bb-man/helper"
	"github.com/alfreddobradi/go-bb-man/parser"

	"github.com/google/uuid"
)

const (
	MaxContentLength = 1024
)

type Status byte

const (
	Waiting Status = iota
	Processing
	OK
	Failed
)

func (s Status) String() string {
	switch s {
	case Waiting:
		return "waiting"
	case Processing:
		return "processing"
	case OK:
		return "ok"
	case Failed:
		return "failed"
	}
	return "unknown"
}

type Task struct {
	ID       uuid.UUID
	Filename string
	Status   Status
	Error    error
}

type Registry struct {
	mx       *sync.Mutex
	globalWg *sync.WaitGroup
	wg       *sync.WaitGroup

	db             database.DB
	done           chan struct{}
	update         chan Update
	tasks          *TaskList
	processedTasks *TaskList
}

type Update struct {
	TaskID uuid.UUID
	Status Status
	Error  error
}

type TaskList struct {
	mx    *sync.Mutex
	tasks map[uuid.UUID]*Task
}

func NewTaskList() *TaskList {
	return &TaskList{
		mx:    &sync.Mutex{},
		tasks: make(map[uuid.UUID]*Task),
	}
}

func (t *TaskList) Add(task *Task) {
	t.mx.Lock()
	defer t.mx.Unlock()
	t.tasks[task.ID] = task
}

func (t *TaskList) Delete(key uuid.UUID) {
	t.mx.Lock()
	defer t.mx.Unlock()
	delete(t.tasks, key)
}

func (t *TaskList) Update(update Update) {
	t.mx.Lock()
	defer t.mx.Unlock()

	task := t.tasks[update.TaskID]
	task.Status = update.Status
	task.Error = update.Error
	t.tasks[update.TaskID] = task
}

func (t *TaskList) Get(key uuid.UUID) *Task {
	t.mx.Lock()
	defer t.mx.Unlock()

	return t.tasks[key]
}

func (t *TaskList) Range(cb func(id uuid.UUID, t *Task)) {
	t.mx.Lock()
	defer t.mx.Unlock()

	for id, task := range t.tasks {
		cb(id, task)
	}
}

func NewRegistry(db database.DB, gwg *sync.WaitGroup) *Registry {
	r := &Registry{
		mx:       &sync.Mutex{},
		globalWg: gwg,
		wg:       &sync.WaitGroup{},

		db: db,

		done:           make(chan struct{}),
		update:         make(chan Update),
		tasks:          NewTaskList(),
		processedTasks: NewTaskList(),
	}

	go func() {
		t := time.NewTicker(time.Second)
		for {
			select {
			case <-t.C:
				log.Printf("Looking for new tasks to pick up")
				r.tasks.Range(func(id uuid.UUID, task *Task) {
					if task.Status == Waiting {
						task.Status = Processing
						r.wg.Add(1)
						go r.processTask(task)
					}
				})
			case <-r.done:
				t.Stop()
				log.Println("Waiting for running tasks to finish")
				r.wg.Wait()
				r.globalWg.Done()
				log.Println("Done")
				return
			case evt := <-r.update:
				task := r.tasks.Get(evt.TaskID)
				r.tasks.Delete(evt.TaskID)
				task.Status = evt.Status
				task.Error = evt.Error
				r.processedTasks.Add(task)
				if evt.Error != nil {
					log.Printf("< Processed task %s with status %s: %v", evt.TaskID.String(), evt.Status.String(), evt.Error)
				} else {
					log.Printf("< Processed task %s with status %s", evt.TaskID.String(), evt.Status.String())
				}
			}
		}
	}()

	return r
}

func (r *Registry) Stop() {
	log.Println("Stopping task watcher")
	r.done <- struct{}{}
}

func (r *Registry) ProcessFile(filename string) error {
	id := uuid.New()
	task := Task{
		ID:       id,
		Filename: filename,
		Status:   Waiting,
	}

	r.tasks.Add(&task)
	return nil
}

func (r *Registry) processTask(t *Task) {
	defer r.wg.Done()
	log.Printf("> Processing %s", t.Filename)

	res, err := zip.OpenReader(t.Filename)
	if err != nil {
		r.update <- Update{
			Status: Failed,
			Error:  err,
		}
		return
	}
	defer res.Close()

	f := res.File[0]

	rc, err := f.Open()
	if err != nil {
		r.update <- Update{
			Status: Failed,
			Error:  err,
		}
		return
	}
	defer rc.Close()

	record, err := parser.Parse(rc)
	if err != nil {
		r.update <- Update{
			TaskID: t.ID,
			Status: Failed,
			Error:  err,
		}
		return
	}

	if err := r.db.SaveReplay(record); err != nil {
		r.update <- Update{
			TaskID: t.ID,
			Status: Failed,
			Error:  err,
		}
		return
	}

	r.update <- Update{
		TaskID: t.ID,
		Status: OK,
		Error:  nil,
	}
}

func (r *Registry) HandleProcessRequest(w http.ResponseWriter, req *http.Request) {
	if err := req.ParseMultipartForm(MaxContentLength); err != nil {
		log.Printf("Failed to process uploaded file: %v", err)
		helper.E(w, http.StatusRequestEntityTooLarge)
		return
	}

	file, handler, err := req.FormFile("replay")
	if err != nil {
		log.Printf("Failed to process uploaded file: %v", err)
		helper.E(w, http.StatusInternalServerError)
		return
	}
	defer file.Close()

	h := sha256.New()
	h.Write([]byte(handler.Filename))
	name := fmt.Sprintf("%x.bbrz", h.Sum(nil))

	resFile, err := os.Create(filepath.Join("/tmp", name))
	if err != nil {
		log.Printf("Failed to process uploaded file: %v", err)
		helper.E(w, http.StatusInternalServerError)
		return
	}

	io.Copy(resFile, file) // nolint
	resFile.Close()

	r.ProcessFile(resFile.Name()) // nolint

	w.Write([]byte("OK")) // nolint
}
