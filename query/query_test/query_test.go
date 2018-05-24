package query

import (
	"bytes"
	"context"
	"io/ioutil"
	"log"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/influxdata/platform"
	"github.com/influxdata/platform/query/csv"
	platformfunctions "github.com/influxdata/platform/query/functions"
	"github.com/influxdata/platform/query/influxql"

	"fmt"

	"github.com/influxdata/platform/query"
	"github.com/influxdata/platform/query/control"
	"github.com/influxdata/platform/query/functions"
	"github.com/influxdata/platform/query/id"

	"strings"

	"github.com/andreyvit/diff"
)

var (
	staticResultID platform.ID
)

func init() {
	staticResultID.DecodeFromString("1")
	query.FinalizeRegistration()
}

// wrapController is needed to make *ifql.Controller implement platform.AsyncQueryService.
// TODO(nathanielc/adam): copied from ifqlde main.go, in which there's a note to remove this type by a better design
type wrapController struct {
	*control.Controller
}

func (c wrapController) Query(ctx context.Context, orgID platform.ID, query *query.Spec) (query.Query, error) {
	q, err := c.Controller.Query(ctx, id.ID(orgID), query)
	return q, err
}

func (c wrapController) QueryWithCompile(ctx context.Context, orgID platform.ID, query string) (query.Query, error) {
	q, err := c.Controller.QueryWithCompile(ctx, id.ID(orgID), query)
	return q, err
}

func Test_QueryEndToEnd(t *testing.T) {
	config := control.Config{
		ConcurrencyQuota: 0,
		MemoryBytesQuota: math.MaxInt64,
	}

	c := control.New(config)

	qs := query.QueryServiceBridge{
		AsyncQueryService: wrapController{Controller: c},
	}

	influxqlTranspiler := influxql.NewTranspiler()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test_cases")
	if err != nil {
		t.Fatal(err)
	}

	ifqlFiles, err := filepath.Glob(filepath.Join(path, "*.ifql"))
	if err != nil {
		t.Fatalf("error searching for ifql files: %s", err)
	}

	for _, ifqlFile := range ifqlFiles {
		ext := filepath.Ext(ifqlFile)
		prefix := ifqlFile[0 : len(ifqlFile)-len(ext)]

		csvIn := prefix + ".in.csv"

		csvOut, err := getTestData(prefix, ".out.csv")
		if err != nil {
			t.Fatalf("error in test case %s: %s", prefix, err)
		}

		ifqlQuery, err := getTestData(prefix, ".ifql")
		if err != nil {
			t.Fatalf("error in test case %s: %s", prefix, err)
		}

		ifqlSpec, err := query.Compile(context.Background(), ifqlQuery)
		if err != nil {
			t.Fatalf("error in test case %s: %s", prefix, err)
		}

		correct, err := QueryTestCheckSpec(t, qs, ifqlSpec, csvIn, csvOut)
		if !correct {
			t.Errorf("failed to run ifql query spec for test case %s. error=%s", prefix, err)
		}

		influxqlQuery, err := getTestData(prefix, ".influxql")
		if err != nil {
			t.Logf("skipping influxql for test case %s: %s", prefix, err)
		} else {
			if err != nil {
				t.Fatalf("error in test case %s: %s", prefix, err)
			}

			influxqlSpec, err := influxqlTranspiler.Transpile(context.Background(), influxqlQuery)
			if err != nil {
				t.Errorf("failed to obtain transpiled influxql query spec for test case %s. error=%s", prefix, err)
			}

			correct, err := QueryTestCheckSpec(t, qs, influxqlSpec, csvIn, csvOut)
			if !correct {
				t.Errorf("failed to run influxql query spec for test case %s. error=%s", prefix, err)
			}
		}
	}
}

func getTestData(prefix, suffix string) (string, error) {
	datafile := prefix + suffix
	csv, err := ioutil.ReadFile(datafile)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %s", datafile)
	}
	return string(csv), nil
}

func ReplaceFromSpec(q *query.Spec, csvSrc string) {
	for _, op := range q.Operations {
		if op.Spec.Kind() == functions.FromKind {
			op.Spec = &platformfunctions.FromCSVOpSpec{
				File: csvSrc,
			}
		}
	}
}

func QueryTestCheckSpec(t *testing.T, qs query.QueryServiceBridge, spec *query.Spec, inputFile, want string) (bool, error) {
	t.Helper()
	ReplaceFromSpec(spec, inputFile)

	//log.Println("QueryTestCheckSpec", query.Formatted(spec, query.FmtJSON))
	log.Println("QueryTestCheckSpec")
	results, err := qs.Query(context.Background(), staticResultID, spec)
	if err != nil {
		t.Errorf("failed to run query spec error=%s", err)
		return false, err
	}

	enc := csv.NewResultEncoder(csv.DefaultEncoderConfig())
	buf := new(bytes.Buffer)
	// we are only expecting one result, for now
	for results.More() {
		res := results.Next()

		err := enc.Encode(buf, res)
		if err != nil {
			t.Errorf("failed to run query spec error=%s", err)
			results.Cancel()
			return false, err
		}

	}

	err = results.Err()
	if err != nil {
		t.Errorf("failed to run query spec error=%s", err)
		return false, err
	}

	got := buf.String()
	if g, w := strings.TrimSpace(got), strings.TrimSpace(want); g != w {
		t.Errorf("Result not as expected want(-) got (+):\n%v", diff.LineDiff(w, g))
		results.Cancel()
		return false, nil
	}

	return true, nil

}
