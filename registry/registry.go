package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/coreos/go-etcd/etcd"
	log "github.com/golang/glog"

	"github.com/coreos/coreinit/job"
	"github.com/coreos/coreinit/machine"
)

const (
	keyPrefix      = "/coreos.com/coreinit/"
	lockPrefix     = "/locks/"
	machinePrefix  = "/machines/"
	requestPrefix  = "/request/"
	schedulePrefix = "/schedule/"
	jobWatchPrefix = "/watch/"
	statePrefix    = "/state/"
)

type Registry struct {
	etcd *etcd.Client
}

func New(client *etcd.Client) (registry *Registry) {
	return &Registry{client}
}

// List the jobs a given Machine is scheduled to run
func (r *Registry) GetScheduledJobs(machine *machine.Machine) map[string]job.Job {
	key := path.Join(keyPrefix, machinePrefix, machine.BootId, schedulePrefix)
	resp, err := r.etcd.Get(key, false, true)

	// Assume the error was KeyNotFound and return an empty data structure
	if err != nil {
		return make(map[string]job.Job, 0)
	}

	jobs := make(map[string]job.Job, len(resp.Node.Nodes))
	for _, kv := range resp.Node.Nodes {
		name := path.Base(kv.Key)

		var payload job.JobPayload
		err := unmarshal(kv.Value, &payload)

		if err == nil {
			j, _ := job.NewJob(name, nil, &payload)
			//FIXME: This will hide duplicate jobs!
			jobs[j.Name] = *j
		} else {
			log.V(1).Infof("Failed to create Job: %s", err)
		}
	}
	return jobs
}

// List the jobs all Machines are scheduled to run
func (r *Registry) GetAllScheduledJobs() map[string]job.Job {
	machines := r.GetActiveMachines()
	jobs := map[string]job.Job{}
	for _, mach := range machines {
		for name, j := range r.GetScheduledJobs(&mach) {
			//FIXME: This will hide duplicate jobs!
			jobs[name] = j
		}
	}
	return jobs
}

// Get the current JobState of the provided Job
func (r *Registry) GetJobState(j *job.Job) *job.JobState {
	key := path.Join(keyPrefix, statePrefix, j.Name)
	resp, err := r.etcd.Get(key, false, true)

	// Assume the error was KeyNotFound and return an empty data structure
	if err != nil {
		return nil
	}

	var state job.JobState
	//TODO: Handle the error generated by unmarshal
	unmarshal(resp.Node.Value, &state)
	return &state
}

// Add a new JobWatch to coreinit
func (r *Registry) AddJobWatch(watch *job.JobWatch) {
	key := path.Join(keyPrefix, jobWatchPrefix, watch.Payload.Name, "object")
	//TODO: Handle the error generated by marshal
	json, _ := marshal(watch)
	r.etcd.Set(key, json, 0)
}

// Attempt to lock a JobWatch on behalf of a Machine
func (r *Registry) ClaimJobWatch(watch *job.JobWatch, m *machine.Machine, ttl time.Duration) bool {
	key := path.Join(keyPrefix, jobWatchPrefix, watch.Payload.Name, "lock")
	return r.acquireLock(key, m.BootId, ttl)
}

// Attempt to remove a given JobWatch from coreinit
func (r *Registry) RemoveJobWatch(name string) {
	key := path.Join(keyPrefix, jobWatchPrefix, name, "object")
	r.etcd.Delete(key, true)

	// Attempt to stop any Jobs that may have been created by the given JobWatch
	for _, m := range r.GetActiveMachines() {
		name := fmt.Sprintf("%s.%s", m.BootId, name)
		j, _ := job.NewJob(name, nil, nil)
		r.RemoveMachineJob(j, &m)
	}
}

// Schedule a Job to a given Machine
func (r *Registry) ScheduleMachineJob(job *job.Job, machine *machine.Machine) {
	key := path.Join(keyPrefix, machinePrefix, machine.BootId, schedulePrefix, job.Name)
	//TODO: Handle the error generated by marshal
	json, _ := marshal(job.Payload)
	r.etcd.Set(key, json, 0)
}

func (r *Registry) GetMachineJob(name string, machine *machine.Machine) *job.Job {
	key := path.Join(keyPrefix, machinePrefix, machine.BootId, schedulePrefix, name)
	resp, err := r.etcd.Get(key, false, true)

	// Assume the error was KeyNotFound and return an empty data structure
	if err != nil {
		return nil
	}

	var jp job.JobPayload
	//TODO: Handle the error generated by unmarshal
	unmarshal(resp.Node.Value, &jp)

	j, _ := job.NewJob(name, nil, &jp)
	return j
}

// RemoveMachineJob removes the Job from any Machine's schedule
func (r *Registry) RemoveMachineJob(job *job.Job, machine *machine.Machine) {
	key := path.Join(keyPrefix, machinePrefix, machine.BootId, schedulePrefix, job.Name)
	r.etcd.Delete(key, true)
}

// Persist the changes in a provided Machine's Job to etcd with the provided TTL
func (r *Registry) SaveJobState(j *job.Job, ttl time.Duration) {
	key := path.Join(keyPrefix, statePrefix, j.Name)
	//TODO: Handle the error generated by marshal
	json, _ := marshal(j.State)
	r.etcd.Set(key, json, uint64(ttl.Seconds()))
}

func (r *Registry) SaveJobWatchState(watch *job.JobWatch, state job.JobWatchState, ttl time.Duration) {
	key := path.Join(keyPrefix, statePrefix, watch.Payload.Name)
	//TODO: Handle the error generated by marshal
	json, _ := marshal(state)
	r.etcd.Set(key, json, uint64(ttl.Seconds()))
}

// Attempt to acquire a lock in etcd on an arbitrary string. Returns true if
// successful, otherwise false.
func (r *Registry) acquireLock(key string, context string, ttl time.Duration) bool {
	resp, err := r.etcd.Get(key, false, true)

	//FIXME: Here lies a race condition!

	if resp != nil {
		if resp.Node.Value == context {
			_, err = r.etcd.Update(key, context, uint64(ttl.Seconds()))
			return err == nil
		}
	}

	_, err = r.etcd.Create(key, context, uint64(ttl.Seconds()))
	return err == nil
}

func marshal(obj interface{}) (string, error) {
	encoded, err := json.Marshal(obj)
	if err == nil {
		return string(encoded), nil
	} else {
		return "", errors.New(fmt.Sprintf("Unable to JSON-serialize object: %s", err))
	}
}

func unmarshal(val string, obj interface{}) error {
	err := json.Unmarshal([]byte(val), &obj)
	if err == nil {
		return nil
	} else {
		return errors.New(fmt.Sprintf("Unable to JSON-deserialize object: %s", err))
	}
}
