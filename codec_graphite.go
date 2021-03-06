package metcap

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type GraphiteCodec struct {
	mutatorRules []GraphiteMutatorRule
	lineRegex    *regexp.Regexp
	fields       [][2]string
}

type GraphiteMutatorRule struct {
	match *regexp.Regexp
	rule  string
}

func NewGraphiteCodec(mutFile string) (GraphiteCodec, error) {
	var mut []GraphiteMutatorRule
	re := regexp.MustCompile(`^(?P<path>[a-zA-Z0-9_\-\.]+) (?P<value>-?[0-9\.]+)(\ (?P<timestamp>[0-9]{10,13}))?$`)

	mutRules, err := os.Open(mutFile)
	if err != nil {
		return GraphiteCodec{}, err
	}

	scn := bufio.NewScanner(mutRules)
	for scn.Scan() {
		rule := strings.Split(scn.Text(), "|||")[0:2]
		ruleRe, err := regexp.Compile(rule[0])
		if err != nil {
			return GraphiteCodec{}, err
		}
		mut = append(mut, GraphiteMutatorRule{ruleRe, rule[1]})
	}

	return GraphiteCodec{
		mutatorRules: mut,
		lineRegex:    re,
	}, nil
}

func (c GraphiteCodec) Decode(input io.Reader) (<-chan *Metric, <-chan error) {
	scn := bufio.NewScanner(input)
	wg := &sync.WaitGroup{}
	metrics := make(chan *Metric)
	errs := make(chan error)

	for scn.Scan() {
		wg.Add(1)
		go func(line string) {
			defer wg.Done()
			// skip empty line
			if line == "" {
				return
			}
			if !c.lineRegex.Match([]byte(line)) {
				return
			}
			// read path, value and optional timestamp into hash map `dissected`
			match := c.lineRegex.FindStringSubmatch(line)
			dissected := map[string]string{}
			for i, n := range c.lineRegex.SubexpNames() {
				dissected[n] = match[i]
			}
			mTimestamp := c.readTimestamp(dissected)
			mValue, err := c.readValue(dissected)
			if err != nil {
				errs <- &CodecError{"Failed to read value", err, dissected["value"]}
				return
			}
			mName, mFields, err := c.readFields(dissected)
			if err != nil {
				errs <- &CodecError{"Failed to read name/fields", err, dissected["path"]}
				return
			}
			metrics <- &Metric{Name: mName, Timestamp: mTimestamp, Value: mValue, Fields: mFields}
		}(scn.Text())
	}

	go func() {
		wg.Wait()
		close(metrics)
		close(errs)
	}()

	return metrics, errs
}

// helper function to parse timestamp into time.Time
func (c GraphiteCodec) readTimestamp(d map[string]string) time.Time {
	var (
		tNow      time.Time
		tByte     []byte
		tLen      int
		tUnixSec  int64
		tUnixNsec int64
		err       error
	)

	tNow = time.Now()
	tByte = []byte(d["timestamp"])
	tLen = len(tByte)

	switch {
	// time not specified
	case tLen == 0:
		return tNow
	// time is in Unix timestamp
	case tLen <= 10:
		tInt, err := strconv.ParseInt(string(tByte), 10, 64)
		if err != nil {
			return tNow
		}
		return time.Unix(tInt, 0)
	// time is in Unix timestamp with second fractions
	case tLen > 10:
		tUnixSec, err = strconv.ParseInt(string(tByte[:10]), 10, 64)
		if err != nil {
			return tNow
		}
		tUnixNsec, err = strconv.ParseInt(string(tByte[10:tLen])+strings.Repeat("0", len(tByte[10:tLen])), 10, 64)
		if err != nil {
			return tNow
		}
		return time.Unix(tUnixSec, tUnixNsec*int64(time.Millisecond))
	default:
		return tNow
	}
}

// helper function to parse value as float64
func (c GraphiteCodec) readValue(d map[string]string) (float64, error) {
	var (
		value float64
		err   error
	)
	if value, err = strconv.ParseFloat(d["value"], 64); err != nil {
		return float64(0), &CodecError{"Failed to parse value", err, d["value"]}
	}
	return value, nil
}

// helper function to parse metric name and fields
func (c GraphiteCodec) readFields(d map[string]string) (string, map[string]string, error) {
	name := []string{}
	fields := make(map[string]string)
	_mutRuleMatch := false
	const stringMatcher string = "qwertyuiopasdfghjklzxcvbnmQWERTYUIOPASDFGHJKLZXCVBNM"
	const numMatcher string = "0123456789"
	// const charMatcher string = "_"

	// check if we have graphite path
	if _, ok := d["path"]; ok {
		// iterate through mutator rules
		for _, mut := range c.mutatorRules {
			// try to match metric path with a mutator rule
			if mut.match.Match([]byte(d["path"])) {
				_mutRuleMatch = true
				fieldValues := strings.Split(d["path"], ".")
				fieldNames := strings.Split(mut.rule, ".")

				// iterate thru fields
			FIELD_PARSER:
				for i, field := range fieldValues {
					switch {
					case fieldNames[i] == "+":
						// catch-all flag -> fill name
						name = append(name, fieldValues[i:]...)
						break FIELD_PARSER
					case fieldNames[i] == "_":
						// no-catch flag -> skip
						continue FIELD_PARSER
					case !strings.ContainsAny(fieldNames[i], stringMatcher) && strings.ContainsAny(fieldNames[i], numMatcher) && strings.HasSuffix(fieldNames[i], "+"):
						name = append(name, fieldValues[i:]...)
						break FIELD_PARSER
					case !strings.ContainsAny(fieldNames[i], stringMatcher) && strings.ContainsAny(fieldNames[i], numMatcher):
						// numeric rule -> name
						name = append(name, field)
					case strings.ContainsAny(fieldNames[i], stringMatcher+numMatcher) && strings.HasSuffix(fieldNames[i], "+"):
						// string rule with catch-all flag -> catch-all field
						f := strings.TrimRight(fieldNames[i], "+")
						fields[f] = strings.Join(fieldValues[i:], "_")
						break FIELD_PARSER
					case strings.ContainsAny(fieldNames[i], stringMatcher+numMatcher):
						// string rule -> field
						fields[fieldNames[i]] = field
					}
				}
				break
			}
		}

		if !_mutRuleMatch {
			name = append(name, strings.Join(strings.Split(d["path"], "."), "_"))
		}
		// not Graphite? then it must be only Influx (for now :))
	} else {
		name = append(name, d["name"])
		// iterate thru fields
		for _, field := range strings.Split(d["fields"], ",") {
			kv := strings.Split(field, "=")
			if kv[0] != "" {
				fields[kv[0]] = kv[1]
			}
		}
	}
	if len(name) == 0 {
		return "", make(map[string]string), &CodecError{"Failed to parse metric name", nil, name}
	}
	return strings.Join(name, ":"), fields, nil
}
