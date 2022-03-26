package parser

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Result represents a test result.
type Result int

// Test result constants
const (
	PASS Result = iota
	FAIL
	SKIP
)

// Report is a collection of package tests.
type Report struct {
	Packages []Package
}

// Package contains the test results of a single package.
type Package struct {
	Name        string
	Duration    time.Duration
	Tests       []*Test
	Benchmarks  []*Benchmark
	CoveragePct string

	// Time is deprecated, use Duration instead.
	Time int // in milliseconds
}

// Test contains the results of a single test.
type Test struct {
	Name     string
	Duration time.Duration
	Result   Result
	Output   []string

	SubtestIndent string

	// Time is deprecated, use Duration instead.
	Time int // in milliseconds
}

// Benchmark contains the results of a single benchmark.
type Benchmark struct {
	Name     string
	Duration time.Duration
	// number of B/op
	Bytes int
	// number of allocs/op
	Allocs int
}

var (
	regexStatus   = regexp.MustCompile(`--- (PASS|FAIL|SKIP): (.+) \((\d+\.\d+)(?: seconds|s)\)`)
	regexIndent   = regexp.MustCompile(`^([ \t]+)---`)
	regexCoverage = regexp.MustCompile(`^coverage:\s+(\d+\.\d+)%\s+of\s+statements(?:\sin\s.+)?$`)
	regexResult   = regexp.MustCompile(`^(ok|FAIL)\s+([^ ]+)\s+(?:(\d+\.\d+)s|\(cached\)|(\[\w+ failed]))(?:\s+coverage:\s+(\d+\.\d+)%\sof\sstatements(?:\sin\s.+)?)?$`)
	// regexBenchmark captures 3-5 groups: benchmark name, number of times ran, ns/op (with or without decimal), B/op (optional), and allocs/op (optional).
	regexBenchmark       = regexp.MustCompile(`^(Benchmark[^ -]+)(?:-\d+\s+|\s+)(\d+)\s+(\d+|\d+\.\d+)\sns/op(?:\s+(\d+)\sB/op)?(?:\s+(\d+)\sallocs/op)?`)
	regexOutput          = regexp.MustCompile(`(    )*\t(.*)`)
	regexSummary         = regexp.MustCompile(`^(PASS|FAIL|SKIP)$`)
	regexPackageWithTest = regexp.MustCompile(`^# ([^\[\]]+) \[[^\]]+\]$`)
)

// Parse parses go test output from reader r and returns a report with the
// results. An optional pkgName can be given, which is used in case a package
// result line is missing.
func Parse(r io.Reader, pkgName string) (*Report, error) {
	reader := bufio.NewReader(r)

	report := &Report{make([]Package, 0)}

	// keep track of tests we find
	var tests []*Test

	// keep track of benchmarks we find
	var benchmarks []*Benchmark

	// sum of tests' time, use this if current test has no result line (when it is compiled test)
	var testsTime time.Duration

	// current test
	var cur string

	// coverage percentage report for current package
	var coveragePct string

	// stores mapping between package name and output of build failures
	var packageCaptures = map[string][]string{}

	// the name of the package which it's build failure output is being captured
	var capturedPackage string

	// capture any non-test output
	var buffers = map[string][]string{}

	// parse lines
	for {
		l, _, err := reader.ReadLine()
		if err != nil && err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}

		line := string(l)

		if strings.HasPrefix(line, "=== RUN ") {
			// new test
			cur = strings.TrimSpace(line[8:])
			tests = append(tests, &Test{
				Name:   cur,
				Result: FAIL,
				Output: make([]string, 0),
			})

			// clear the current build package, so output lines won't be added to that build
			capturedPackage = ""
		} else if matches := regexBenchmark.FindStringSubmatch(line); len(matches) == 6 {
			bytes, _ := strconv.Atoi(matches[4])
			allocs, _ := strconv.Atoi(matches[5])

			benchmarks = append(benchmarks, &Benchmark{
				Name:     matches[1],
				Duration: parseNanoseconds(matches[3]),
				Bytes:    bytes,
				Allocs:   allocs,
			})
		} else if strings.HasPrefix(line, "=== PAUSE ") {
			continue
		} else if strings.HasPrefix(line, "=== CONT ") {
			cur = strings.TrimSpace(line[8:])
			continue
		} else if matches := regexResult.FindStringSubmatch(line); len(matches) == 6 {
			if matches[5] != "" {
				coveragePct = matches[5]
			}
			if strings.HasSuffix(matches[4], "failed]") {
				// the build of the package failed, inject a dummy test into the package
				// which indicate about the failure and contain the failure description.
				tests = append(tests, &Test{
					Name:   matches[4],
					Result: FAIL,
					Output: packageCaptures[matches[2]],
				})
			} else if matches[1] == "FAIL" && !containsFailures(tests) && len(buffers[cur]) > 0 {
				// This package didn't have any failing tests, but still it
				// failed with some output. Create a dummy test with the
				// output.
				tests = append(tests, &Test{
					Name:   "Failure",
					Result: FAIL,
					Output: buffers[cur],
				})
				buffers[cur] = buffers[cur][0:0]
			}

			// all tests in this package are finished
			report.Packages = append(report.Packages, Package{
				Name:        matches[2],
				Duration:    parseSeconds(matches[3]),
				Tests:       tests,
				Benchmarks:  benchmarks,
				CoveragePct: coveragePct,

				Time: int(parseSeconds(matches[3]) / time.Millisecond), // deprecated
			})

			buffers[cur] = buffers[cur][0:0]
			tests = make([]*Test, 0)
			benchmarks = make([]*Benchmark, 0)
			coveragePct = ""
			cur = ""
			testsTime = 0
		} else if matches := regexStatus.FindStringSubmatch(line); len(matches) == 4 {
			cur = matches[2]
			test := findTest(tests, cur)
			if test == nil {
				continue
			}

			// test status
			if matches[1] == "PASS" {
				test.Result = PASS
			} else if matches[1] == "SKIP" {
				test.Result = SKIP
			} else {
				test.Result = FAIL
			}

			if matches := regexIndent.FindStringSubmatch(line); len(matches) == 2 {
				test.SubtestIndent = matches[1]
			}

			test.Output = buffers[cur]

			test.Name = matches[2]
			test.Duration = parseSeconds(matches[3])
			testsTime += test.Duration

			test.Time = int(test.Duration / time.Millisecond) // deprecated
		} else if matches := regexCoverage.FindStringSubmatch(line); len(matches) == 2 {
			coveragePct = matches[1]
		} else if matches := regexOutput.FindStringSubmatch(line); capturedPackage == "" && len(matches) == 3 {
			// Sub-tests start with one or more series of 4-space indents, followed by a hard tab,
			// followed by the test output
			// Top-level tests start with a hard tab.
			test := findTest(tests, cur)
			if test == nil {
				continue
			}
			test.Output = append(test.Output, matches[2])
		} else if strings.HasPrefix(line, "# ") {
			// indicates a capture of build output of a package. set the current build package.
			packageWithTestBinary := regexPackageWithTest.FindStringSubmatch(line)
			if packageWithTestBinary != nil {
				// Sometimes, the text after "# " shows the name of the test binary
				// ("<package>.test") in addition to the package
				// e.g.: "# package/name [package/name.test]"
				capturedPackage = packageWithTestBinary[1]
			} else {
				capturedPackage = line[2:]
			}
		} else if capturedPackage != "" {
			// current line is build failure capture for the current built package
			packageCaptures[capturedPackage] = append(packageCaptures[capturedPackage], line)
		} else if regexSummary.MatchString(line) {
			// unset current test name so any additional output after the
			// summary is captured separately.
			cur = ""
		} else {
			// buffer anything else that we didn't recognize
			buffers[cur] = append(buffers[cur], line)

			// if we have a current test, also append to its output
			test := findTest(tests, cur)
			if test != nil {
				if strings.HasPrefix(line, test.SubtestIndent+"    ") {
					test.Output = append(test.Output, strings.TrimPrefix(line, test.SubtestIndent+"    "))
				}
			}
		}
	}

	if len(tests) > 0 {
		// no result line found
		report.Packages = append(report.Packages, Package{
			Name:        pkgName,
			Duration:    testsTime,
			Time:        int(testsTime / time.Millisecond),
			Tests:       tests,
			Benchmarks:  benchmarks,
			CoveragePct: coveragePct,
		})
	}

	return report, nil
}

func parseSeconds(t string) time.Duration {
	if t == "" {
		return time.Duration(0)
	}
	// ignore error
	d, _ := time.ParseDuration(t + "s")
	return d
}

func parseNanoseconds(t string) time.Duration {
	// note: if input < 1 ns precision, result will be 0s.
	if t == "" {
		return time.Duration(0)
	}
	// ignore error
	d, _ := time.ParseDuration(t + "ns")
	return d
}

func findTest(tests []*Test, name string) *Test {
	for i := len(tests) - 1; i >= 0; i-- {
		if tests[i].Name == name {
			return tests[i]
		}
	}
	return nil
}

func containsFailures(tests []*Test) bool {
	for _, test := range tests {
		if test.Result == FAIL {
			return true
		}
	}
	return false
}

// Failures counts the number of failed tests in this report
func (r *Report) Failures() int {
	count := 0

	for _, p := range r.Packages {
		for _, t := range p.Tests {
			if t.Result == FAIL {
				count++
			}
		}
	}

	return count
}

type TestEvent struct {
	Time time.Time // encodes as an RFC3339-format string
	//    run    - the test has started running
	//    pause  - the test has been paused
	//    cont   - the test has continued running
	//    pass   - the test passed
	//    bench  - the benchmark printed log output but did not fail
	//    fail   - the test or benchmark failed
	//    output - the test printed output
	//    skip   - the test was skipped or the package contained no tests
	Action  string
	Package string
	Test    string
	Elapsed float64 // seconds
	Output  string
}

type status struct {
	other  map[string]TestEvent
	output []string
}

func ParseJson(r io.Reader, pkgName string) (*Report, error) {
	de := json.NewDecoder(r)

	report := Report{}
	pkgIdx := make(map[string]int)
	eventIdx := make(map[[2]string]*status) // Package,Test
	for {
		event := TestEvent{}
		err := de.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}

		if _, ok := pkgIdx[event.Package]; !ok {
			report.Packages = append(report.Packages, Package{
				Name: event.Package,
			})
			pkgIdx[event.Package] = len(report.Packages) - 1
		}

		s, ok := eventIdx[[2]string{event.Package, event.Test}]
		if !ok {
			s = &status{
				other: make(map[string]TestEvent),
			}
			eventIdx[[2]string{event.Package, event.Test}] = s
		}

		if event.Action != "output" {
			s.other[event.Action] = event
			continue
		}

		if strings.HasPrefix(event.Output, "--- ") || strings.HasPrefix(event.Output, "=== ") {
			continue
		}

		output := strings.TrimPrefix(event.Output, "\t")
		s.output = append(s.output, strings.TrimSuffix(output, "\n"))
	}

	for k, s := range eventIdx {
		pkg := &report.Packages[pkgIdx[k[0]]]
		// package
		if k[1] == "" {
			updatePackage(pkg, s)
			continue
		}

		// test
		addTest(pkg, s)
	}

	return &report, nil
}

func updatePackage(pkg *Package, s *status) {
	for _, ac := range []string{"pass", "skip", "fail"} {
		if ev, ok := s.other[ac]; ok {
			pkg.Duration = time.Duration(ev.Elapsed*1e6) * time.Microsecond
			pkg.Time = int(ev.Elapsed * 1e3)
			return
		}
	}
}

func addTest(pkg *Package, s *status) {
	if _, ok := s.other["run"]; !ok {
		return
	}

	acMap := []Result{PASS, SKIP, FAIL}
	for i, ac := range []string{"pass", "skip", "fail"} {
		ev, ok := s.other[ac]
		if !ok {
			continue
		}

		pkg.Tests = append(pkg.Tests, &Test{
			Name:          ev.Test,
			Duration:      time.Duration(ev.Elapsed*1e6) * time.Microsecond,
			Result:        acMap[i],
			Output:        s.output,
			SubtestIndent: "",
			Time:          int(ev.Elapsed * 1e3),
		})
		return
	}
}
