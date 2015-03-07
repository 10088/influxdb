package influxql

import (
	"bytes"
	"hash/fnv"
	"sort"
	"time"
)

// DB represents an interface for creating transactions.
type DB interface {
	Begin() (Tx, error)
}

// Tx represents a transaction.
// The Tx must be opened before being used.
type Tx interface {
	// Create MapReduceJobs for the given select statement. One MRJob will be created per unique tagset that matches the query
	CreateMapReduceJobs(stmt *SelectStatement, tagKeys []string) ([]*MapReduceJob, error)
}

type MapReduceJob struct {
	MeasurementName string
	TagSet          *TagSet
	Mappers         []Mapper         // the mappers to hit all shards for this MRJob
	TMin            int64            // minimum time specified in the query
	TMax            int64            // maximum time specified in the query
	key             []byte           // a key that identifies the MRJob so it can be sorted
	interval        int64            // the group by interval of the query
	stmt            *SelectStatement // the select statement this job was created for
}

func (m *MapReduceJob) Open() error {
	for _, mm := range m.Mappers {
		if err := mm.Open(); err != nil {
			m.Close()
			return err
		}
	}
	return nil
}

func (m *MapReduceJob) Close() {
	for _, mm := range m.Mappers {
		mm.Close()
	}
}

func (m *MapReduceJob) Key() []byte {
	if m.key == nil {
		m.key = append([]byte(m.MeasurementName), m.TagSet.Key...)
	}
	return m.key
}

func (m *MapReduceJob) Execute(out chan *Row) {
	warn("Execute0")
	aggregates := m.stmt.AggregateCalls()
	reduceFuncs := make([]ReduceFunc, len(aggregates))
	for i, c := range aggregates {
		warn("Execute0.1 ", c.String())
		reduceFunc, err := InitializeReduceFunc(c)
		if err != nil {
			out <- &Row{Err: err}
			return
		}
		reduceFuncs[i] = reduceFunc
	}

	isRaw := false
	// modify if it's a raw data query
	if len(aggregates) == 0 {
		isRaw = true
		aggregates = []*Call{nil}
		r, _ := InitializeReduceFunc(nil)
		reduceFuncs = append(reduceFuncs, r)
	}

	warn("Execute1: ", len(m.Mappers))
	// we'll have a fixed number of points with timestamps in buckets. Initialize those times and a slice to hold the associated values
	var pointCountInResult int

	// if the user didn't specify a start time or a group by interval, we're returning a single point that describes the entire range
	if m.TMin == 0 || m.interval == 0 || isRaw {
		// they want a single aggregate point for the entire time range
		m.interval = m.TMax - m.TMin
		pointCountInResult = 1
	} else {
		intervalTop := m.TMax/m.interval*m.interval + m.interval
		warn("ex: ", intervalTop, intervalTop-m.TMax, m.interval)
		pointCountInResult = int((intervalTop - m.TMin) / m.interval)
	}

	warn("Execute1.1 ", pointCountInResult, m.TMin, m.TMax)

	// initialize the times of the aggregate points
	resultTimes := make([]int64, pointCountInResult)
	resultValues := make([][]interface{}, pointCountInResult)
	for i, _ := range resultTimes {
		t := m.TMin + (int64(i) * m.interval)
		resultTimes[i] = t
		vals := make([]interface{}, 0, len(aggregates)+1)
		resultValues[i] = append(vals, time.Unix(0, t).UTC())
	}

	warn("Execute2")
	// now loop through the aggregate functions and populate everything
	for i, c := range aggregates {
		if err := m.processAggregate(c, reduceFuncs[i], resultValues); err != nil {
			out <- &Row{
				Name: m.MeasurementName,
				Tags: m.TagSet.Tags,
				Err:  err,
			}

			return
		}
	}

	if isRaw {
		out <- m.processRawResults(resultValues)
		return
	}

	// put together the row to return
	columnNames := make([]string, len(aggregates)+1)
	columnNames[0] = "time"
	for i, a := range aggregates {
		columnNames[i+1] = a.Name
	}

	row := &Row{
		Name:    m.MeasurementName,
		Tags:    m.TagSet.Tags,
		Columns: columnNames,
		Values:  resultValues,
	}

	// and we out
	out <- row
}

// processRawResults will handle converting the reduce results from a raw query into a Row
func (m *MapReduceJob) processRawResults(resultValues [][]interface{}) *Row {
	selectNames := m.stmt.NamesInSelect()
	hasTime := false
	for _, n := range selectNames {
		if n == "time" {
			hasTime = true
		}
	}

	// time should always be in the list of names they get back
	if !hasTime {
		selectNames = append([]string{"time"}, selectNames...)
	}

	// if they've selected only a single value we have to handle things a little differently
	singleValue := len(selectNames) == 2

	warn("0> ", selectNames)
	row := &Row{
		Name:    m.MeasurementName,
		Tags:    m.TagSet.Tags,
		Columns: selectNames,
	}

	// return an empty row if there are no results
	if len(resultValues) == 0 || len(resultValues[0]) != 2 {
		warn("1> empty resaults ", len(resultValues[0]))
		return row
	}

	// the results will hav all of the raw mapper results, convert into the row
	for _, v := range resultValues[0][1].([]*rawQueryMapOutput) {
		warn("2> ", v)
		vals := make([]interface{}, len(selectNames))

		if singleValue {
			warn("3> ")
			vals[0] = time.Unix(0, v.timestamp).UTC()
			vals[1] = v.values.(interface{})
		} else {
			fields := v.values.(map[string]interface{})
			for i, n := range selectNames {
				if n == "time" {
					vals[i] = time.Unix(0, v.timestamp).UTC()
				} else {
					vals[i] = fields[n]
				}
			}
		}

		row.Values = append(row.Values, vals)
	}

	return row
}

func (m *MapReduceJob) processAggregate(c *Call, reduceFunc ReduceFunc, resultValues [][]interface{}) error {
	warn("processAggregate0")
	mapperOutputs := make([]interface{}, len(m.Mappers))

	warn("processAggregate1")
	// intialize the mappers
	for _, mm := range m.Mappers {
		if err := mm.Begin(c, m.TMin); err != nil {
			return err
		}
	}

	// populate the result values for each interval of time
	for i, _ := range resultValues {
		warn("processAggregate2 ", i, len(m.Mappers))
		// collect the results from each mapper
		for j, mm := range m.Mappers {
			res, err := mm.NextInterval(m.interval)
			if err != nil {
				return err
			}
			mapperOutputs[j] = res
		}
		warn("processAggregate2 ", i)
		resultValues[i] = append(resultValues[i], reduceFunc(mapperOutputs))
	}

	return nil
}

type MapReduceJobs []*MapReduceJob

func (a MapReduceJobs) Len() int           { return len(a) }
func (a MapReduceJobs) Less(i, j int) bool { return bytes.Compare(a[i].Key(), a[j].Key()) == -1 }
func (a MapReduceJobs) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// Mapper will run through a map function. A single mapper will be created
// for each shard for each tagset that must be hit to satisfy a query.
// Mappers can either point to a local shard or could point to a remote server.
type Mapper interface {
	// Open will open the necessary resources to being the map job. Could be connections to remote servers or
	// hitting the local bolt store
	Open() error

	// Close will close the mapper (either the bolt transaction or the request)
	Close()

	// Begin will set up the mapper to run the map function for a given aggregate call starting at the passed in time
	Begin(*Call, int64) error

	// NextInterval will get the time ordered next interval of the given interval size from the mapper. This is a
	// forward only operation from the start time passed into Begin. Will return nil when there is no more data to be read.
	// We pass the interval in here so that it can be varied over the period of the query. This is useful for the raw
	// data queries where we'd like to gradually adjust the amount of time we scan over.
	NextInterval(interval int64) (interface{}, error)
}

type TagSet struct {
	Tags      map[string]string
	Filters   []Expr
	SeriesIDs []uint32
	Key       []byte
}

func (t *TagSet) AddFilter(id uint32, filter Expr) {
	t.SeriesIDs = append(t.SeriesIDs, id)
	t.Filters = append(t.Filters, filter)
}

// Planner represents an object for creating execution plans.
type Planner struct {
	DB DB

	// Returns the current time. Defaults to time.Now().
	Now func() time.Time
}

// NewPlanner returns a new instance of Planner.
func NewPlanner(db DB) *Planner {
	return &Planner{
		DB:  db,
		Now: time.Now,
	}
}

// Plan creates an execution plan for the given SelectStatement and returns an Executor.
func (p *Planner) Plan(stmt *SelectStatement) (*Executor, error) {
	now := p.Now()

	// Replace instances of "now()" with the current time.
	stmt.Condition = Reduce(stmt.Condition, &nowValuer{Now: now})

	// Begin an unopened transaction.
	tx, err := p.DB.Begin()
	if err != nil {
		return nil, err
	}

	// Determine group by tag keys.
	interval, tags, err := stmt.Dimensions.Normalize()
	if err != nil {
		return nil, err
	}

	// TODO: hanldle queries that select from multiple measurements. This assumes that we're only selecting from a single one
	jobs, err := tx.CreateMapReduceJobs(stmt, tags)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		j.interval = interval.Nanoseconds()
		j.stmt = stmt
	}

	return &Executor{tx: tx, stmt: stmt, jobs: jobs, interval: interval.Nanoseconds()}, nil
}

// Executor represents the implementation of Executor.
// It executes all reducers and combines their result into a row.
type Executor struct {
	tx       Tx               // transaction
	stmt     *SelectStatement // original statement
	jobs     []*MapReduceJob  // one job per unique tag set that will return in the query
	interval int64            // the group by interval of the query in nanoseconds
}

// Execute begins execution of the query and returns a channel to receive rows.
func (e *Executor) Execute() (<-chan *Row, error) {
	// Open transaction.
	for _, j := range e.jobs {
		if err := j.Open(); err != nil {
			e.close()
			return nil, err
		}
	}

	// Create output channel and stream data in a separate goroutine.
	out := make(chan *Row, 0)
	go e.execute(out)

	return out, nil
}

func (e *Executor) close() {
	for _, j := range e.jobs {
		j.Close()
	}
}

// execute runs in a separate separate goroutine and streams data from processors.
func (e *Executor) execute(out chan *Row) {
	// Ensure the the MRJobs close after execution.
	defer e.close()

	// Execute each MRJob serially
	for _, j := range e.jobs {
		j.Execute(out)
	}

	// Mark the end of the output channel.
	close(out)
}

// Row represents a single row returned from the execution of a statement.
type Row struct {
	Name    string            `json:"name,omitempty"`
	Tags    map[string]string `json:"tags,omitempty"`
	Columns []string          `json:"columns"`
	Values  [][]interface{}   `json:"values,omitempty"`
	Err     error             `json:"err,omitempty"`
}

// tagsHash returns a hash of tag key/value pairs.
func (r *Row) tagsHash() uint64 {
	h := fnv.New64a()
	keys := r.tagsKeys()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte(r.Tags[k]))
	}
	return h.Sum64()
}

// tagKeys returns a sorted list of tag keys.
func (r *Row) tagsKeys() []string {
	a := make([]string, len(r.Tags))
	for k := range r.Tags {
		a = append(a, k)
	}
	sort.Strings(a)
	return a
}

// Rows represents a list of rows that can be sorted consistently by name/tag.
type Rows []*Row

func (p Rows) Len() int { return len(p) }

func (p Rows) Less(i, j int) bool {
	// Sort by name first.
	if p[i].Name != p[j].Name {
		return p[i].Name < p[j].Name
	}

	// Sort by tag set hash. Tags don't have a meaningful sort order so we
	// just compute a hash and sort by that instead. This allows the tests
	// to receive rows in a predictable order every time.
	return p[i].tagsHash() < p[j].tagsHash()
}

func (p Rows) Swap(i, j int) { p[i], p[j] = p[j], p[i] }
