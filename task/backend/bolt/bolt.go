// Package bolt provides an bolt-backed store implementation.
//
// The data stored in bolt is structured as follows:
//
//    bucket(/tasks/v1/tasks) key(:task_id) -> Content of submitted task (i.e. flux code).
//    bucket(/tasks/v1/task_meta) Key(:task_id) -> Protocol Buffer encoded pb.StoredTaskInternalMeta,
//                                    so we have a consistent view of runs in progress and max concurrency.
//    bucket(/tasks/v1/org_by_task_id) key(task_id) -> The organization ID (stored as encoded string) associated with given task.
//    bucket(/tasks/v1/user_by_task_id) key(:task_id) -> The user ID (stored as encoded string) associated with given task.
//    buket(/tasks/v1/name_by_task_id) key(:task_id) -> The user-supplied name of the script.
//                                         Maybe we don't need this after name becomes a script option?
//                                         Or maybe we do need it as part of ensuring uniqueness.
//    bucket(/tasks/v1/run_ids) -> Counter for run IDs
//    bucket(/tasks/v1/orgs).bucket(:org_id) key(:task_id) -> Empty content; presence of :task_id allows for lookup from org to tasks.
//    bucket(/tasks/v1/users).bucket(:user_id) key(:task_id) -> Empty content; presence of :task_id allows for lookup from user to tasks.
//
// Note that task IDs are stored big-endian uint64s for sorting purposes,
// but presented to the users with leading 0-bytes stripped.
// Like other components of the system, IDs presented to users may be `0f12` rather than `f12`.
package bolt

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	bolt "github.com/coreos/bbolt"
	"github.com/influxdata/platform"
	"github.com/influxdata/platform/task/backend"
	"github.com/influxdata/platform/task/backend/pb"
)

// ErrDBReadOnly is an error for when the database is set to read only.
// Tasks needs to be able to write to the db.
var ErrDBReadOnly = errors.New("db is read only")

// ErrMaxConcurrency is an error for when the max concurrency is already
// reached for a task when you try to schedule a task.
var ErrMaxConcurrency = errors.New("MaxConcurrency reached")

// ErrRunNotFound is an error for when a run isn't found in a FinishRun method.
var ErrRunNotFound = errors.New("run not found")

// ErrNotFound is an error for when a task could not be found
var ErrNotFound = errors.New("task not found")

// Store is task store for bolt.
type Store struct {
	db     *bolt.DB
	bucket []byte
}

const basePath = "/tasks/v1/"

var (
	tasksPath    = []byte(basePath + "tasks")
	orgsPath     = []byte(basePath + "orgs")
	usersPath    = []byte(basePath + "users")
	taskMetaPath = []byte(basePath + "task_meta")
	orgByTaskID  = []byte(basePath + "org_by_task_id")
	userByTaskID = []byte(basePath + "user_by_task_id")
	nameByTaskID = []byte(basePath + "name_by_task_id")
	runIDs       = []byte(basePath + "run_ids")
)

// New gives us a new Store based on "github.com/coreos/bbolt"
func New(db *bolt.DB, rootBucket string) (*Store, error) {
	if db.IsReadOnly() {
		return nil, ErrDBReadOnly
	}
	bucket := []byte(rootBucket)

	err := db.Update(func(tx *bolt.Tx) error {
		// create root
		root, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
		// create the buckets inside the root
		for _, b := range [][]byte{
			tasksPath, orgsPath, usersPath, taskMetaPath,
			orgByTaskID, userByTaskID, nameByTaskID, runIDs,
		} {
			_, err := root.CreateBucketIfNotExists(b)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Store{db: db, bucket: bucket}, nil
}

// CreateTask creates a task in the boltdb task store.
func (s *Store) CreateTask(ctx context.Context, org, user platform.ID, script string) (platform.ID, error) {
	o, err := backend.StoreValidator.CreateArgs(org, user, script)
	if err != nil {
		return nil, err
	}

	id := make(platform.ID, 8)

	err = s.db.Update(func(tx *bolt.Tx) error {
		// get the root bucket
		b := tx.Bucket(s.bucket)
		// Get ID
		idi, _ := b.NextSequence() // we ignore this err check, because this can't err inside an Update call
		binary.BigEndian.PutUint64(id, idi)

		// write script
		err := b.Bucket(tasksPath).Put(id, []byte(script))
		if err != nil {
			return err
		}

		// name
		err = b.Bucket(nameByTaskID).Put(id, []byte(o.Name))
		if err != nil {
			return err
		}

		// org
		orgB, err := b.Bucket(orgsPath).CreateBucketIfNotExists([]byte(org))
		if err != nil {
			return err
		}

		err = orgB.Put(id, nil)
		if err != nil {
			return err
		}

		err = b.Bucket(orgByTaskID).Put(id, []byte(org))
		if err != nil {
			return err
		}

		// user
		userB, err := b.Bucket(usersPath).CreateBucketIfNotExists([]byte(user))
		if err != nil {
			return err
		}

		err = userB.Put(id, nil)
		if err != nil {
			return err
		}

		err = b.Bucket(userByTaskID).Put(id, []byte(user))
		if err != nil {
			return err
		}

		// metadata
		stm := pb.StoredTaskInternalMeta{
			MaxConcurrency: 1,
		}

		stmBytes, err := stm.Marshal()
		if err != nil {
			return err
		}
		metaB := b.Bucket(taskMetaPath)
		return metaB.Put(id, stmBytes)
	})
	if err != nil {
		return nil, err
	}
	return unpadID(id), nil
}

// ModifyTask changes a task with a new script, it should error if the task does not exist.
func (s *Store) ModifyTask(ctx context.Context, id platform.ID, newScript string) error {
	if _, err := backend.StoreValidator.ModifyArgs(id, newScript); err != nil {
		return err
	}

	paddedID := padID(id)
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket).Bucket(tasksPath)
		if v := b.Get(paddedID); v == nil { // this is so we can error if the task doesn't exist
			return ErrNotFound
		}
		return b.Put(paddedID, []byte(newScript))
	})
}

// ListTasks lists the tasks based on a filter.
func (s *Store) ListTasks(ctx context.Context, params backend.TaskSearchParams) ([]backend.StoreTask, error) {
	if len(params.Org) > 0 && len(params.User) > 0 {
		return nil, errors.New("ListTasks: org and user filters are mutually exclusive")
	}

	const (
		defaultPageSize = 100
		maxPageSize     = 500
	)
	if params.PageSize < 0 {
		return nil, errors.New("ListTasks: PageSize must be positive")
	}
	if params.PageSize > maxPageSize {
		return nil, fmt.Errorf("ListTasks: PageSize exceeds maximum of %d", maxPageSize)
	}
	lim := params.PageSize
	if lim == 0 {
		lim = defaultPageSize
	}
	taskIDs := make([]platform.ID, 0, params.PageSize)

	err := s.db.View(func(tx *bolt.Tx) error {
		var c *bolt.Cursor
		b := tx.Bucket(s.bucket)
		if len(params.Org) > 0 {
			orgB := b.Bucket(orgsPath).Bucket(params.Org)
			if orgB == nil {
				return ErrNotFound
			}
			c = orgB.Cursor()
		} else if len(params.User) > 0 {
			userB := b.Bucket(usersPath).Bucket(params.User)
			if userB == nil {
				return ErrNotFound
			}
			c = userB.Cursor()
		} else {
			c = b.Bucket(tasksPath).Cursor()
		}
		if len(params.After) > 0 {
			c.Seek(padID(params.After))
			for k, _ := c.Next(); k != nil && len(taskIDs) < lim; k, _ = c.Next() {
				taskIDs = append(taskIDs, k)
			}
			return nil
		}
		for k, _ := c.First(); k != nil && len(taskIDs) < lim; k, _ = c.Next() {
			taskIDs = append(taskIDs, k)
		}
		return nil
	})
	if err == ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// now lookup each task
	tasks := make([]backend.StoreTask, len(taskIDs))
	if err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		for i := range taskIDs {
			// TODO(docmerlin): optimization: don't check <-ctx.Done() every time though the loop
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				// TODO(docmerlin): change the setup to reduce the number of lookups to 1 or 2.
				paddedID := taskIDs[i]
				tasks[i].ID = unpadID(paddedID)
				tasks[i].Script = string(b.Bucket(tasksPath).Get(paddedID))
				tasks[i].Name = string(b.Bucket(nameByTaskID).Get(paddedID))
			}
		}
		if len(params.Org) > 0 {
			for i := range taskIDs {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					paddedID := taskIDs[i]
					tasks[i].Org = params.Org
					tasks[i].User = b.Bucket(userByTaskID).Get(paddedID)
				}
			}
			return nil
		}
		if len(params.User) > 0 {
			for i := range taskIDs {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					paddedID := taskIDs[i]
					tasks[i].User = params.User
					tasks[i].Org = b.Bucket(orgByTaskID).Get(paddedID)
				}
			}
			return nil
		}
		for i := range taskIDs {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				paddedID := taskIDs[i]
				tasks[i].User = b.Bucket(userByTaskID).Get(paddedID)
				tasks[i].Org = b.Bucket(orgByTaskID).Get(paddedID)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return tasks, nil
}

// FindTaskByID finds a task with a given an ID.  It will return nil if the task does not exist.
func (s *Store) FindTaskByID(ctx context.Context, id platform.ID) (*backend.StoreTask, error) {
	var stmBytes []byte
	var script []byte
	var userID []byte
	var name []byte
	var org []byte
	paddedID := padID(id)
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		script = b.Bucket(tasksPath).Get(paddedID)
		if script == nil {
			return ErrNotFound
		}
		stmBytes = b.Bucket(taskMetaPath).Get(paddedID)
		userID = b.Bucket(userByTaskID).Get(paddedID)
		name = b.Bucket(nameByTaskID).Get(paddedID)
		org = b.Bucket(orgByTaskID).Get(paddedID)
		return nil
	})
	if err == ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	stm := pb.StoredTaskInternalMeta{}
	err = stm.Unmarshal(stmBytes)
	if err != nil {
		return nil, err
	}

	return &backend.StoreTask{
		ID:     append([]byte(nil), id...), // copy of input id
		Org:    org,
		User:   userID,
		Name:   string(name),
		Script: string(script),
	}, err
}

// DeleteTask deletes the task
func (s *Store) DeleteTask(ctx context.Context, id platform.ID) (deleted bool, err error) {
	paddedID := padID(id)
	err = s.db.Batch(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		if check := b.Bucket(tasksPath).Get(paddedID); check == nil {
			return ErrNotFound
		}
		if err := b.Bucket(taskMetaPath).Delete(paddedID); err != nil {
			return err
		}
		if err := b.Bucket(tasksPath).Delete(paddedID); err != nil {
			return err
		}
		user := b.Bucket(userByTaskID).Get(paddedID)
		if len(user) > 0 {
			if err := b.Bucket(usersPath).Bucket(user).Delete(paddedID); err != nil {
				return err
			}
		}
		if err := b.Bucket(userByTaskID).Delete(paddedID); err != nil {
			return err
		}
		if err := b.Bucket(nameByTaskID).Delete(paddedID); err != nil {
			return err
		}

		org := b.Bucket(orgByTaskID).Get(paddedID)
		if len(org) > 0 {
			if err := b.Bucket(orgsPath).Bucket(org).Delete(paddedID); err != nil {
				return err
			}
		}
		return b.Bucket(orgByTaskID).Delete(paddedID)
	})
	if err == ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// CreateRun adds `now` to the task's metaData if we have not exceeded 'max_concurrency'.
func (s *Store) CreateRun(ctx context.Context, taskID platform.ID, now int64) (backend.QueuedRun, error) {
	queuedRun := backend.QueuedRun{TaskID: append([]byte(nil), taskID...), Now: now}
	stm := pb.StoredTaskInternalMeta{}
	paddedID := padID(taskID)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		stmBytes := b.Bucket(taskMetaPath).Get(paddedID)
		if err := stm.Unmarshal(stmBytes); err != nil {
			return err
		}
		if len(stm.CurrentlyRunning) >= int(stm.MaxConcurrency) {
			return ErrMaxConcurrency
		}
		intID, err := b.Bucket(runIDs).NextSequence()
		if err != nil {
			return err
		}

		running := &pb.StoredTaskInternalMeta_RunningList{
			NowTimestampUnix: now,
			Try:              1,
			RunID:            intID,
		}

		stm.CurrentlyRunning = append(stm.CurrentlyRunning, running)
		stmBytes, err = stm.Marshal()
		if err != nil {
			return err
		}

		var runID [8]byte
		binary.BigEndian.PutUint64(runID[:], intID)
		queuedRun.RunID = unpadID(runID[:])

		return tx.Bucket(s.bucket).Bucket(taskMetaPath).Put(paddedID, stmBytes)
	}); err != nil {
		return queuedRun, err
	}

	return queuedRun, nil
}

// FinishRun removes runID from the list of running tasks and if its `now` is later then last completed update it.
func (s *Store) FinishRun(ctx context.Context, taskID, runID platform.ID) error {
	stm := pb.StoredTaskInternalMeta{}
	paddedID := padID(taskID)

	intID := binary.BigEndian.Uint64(padID(runID))

	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(s.bucket)
		stmBytes := b.Bucket(taskMetaPath).Get(paddedID)
		if err := stm.Unmarshal(stmBytes); err != nil {
			return err
		}
		found := false
		for i, runner := range stm.CurrentlyRunning {
			if runner.RunID == intID {
				found = true
				stm.CurrentlyRunning = append(stm.CurrentlyRunning[:i], stm.CurrentlyRunning[i+1:]...)
				if runner.NowTimestampUnix > stm.LastCompletedTimestampUnix {
					stm.LastCompletedTimestampUnix = runner.NowTimestampUnix
					break
				}
			}
		}
		if !found {
			return ErrRunNotFound
		}

		stmBytes, err := stm.Marshal()
		if err != nil {
			return err
		}

		return tx.Bucket(s.bucket).Bucket(taskMetaPath).Put(paddedID, stmBytes)
	})
}

// Close closes the store
func (s *Store) Close() error {
	return s.db.Close()
}

// unpadID returns a copy of id with leading 0-bytes removed.
// This allows user-facing IDs to look prettier.
func unpadID(id platform.ID) platform.ID {
	trimmed := bytes.TrimLeft(id, "\x00")
	return append([]byte(nil), trimmed...)
}

// padID returns an id, copying it and padding it with leading `0` bytes, if it is less than 8 long.
// it does not copy the id if it is already 8 long
// This allows us to accept pretty user-facing IDs but pad them internally for boltdb sorting.
func padID(id platform.ID) platform.ID {
	if len(id) >= 8 {
		// don't pad if the id is long enough
		return id
	}

	var buf [8]byte
	copy(buf[len(buf)-len(id):], id)
	return buf[:]
}
