// Copyright © 2015 Erik Brady <brady@dvln.org>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package out is for easy and flexible CLI output and log handling.  Goals:
//
// - Leveled output: trace, debug, verbose, print, note, issue, error, fatal
//
// - Out of box for basic screen output (stdout/stderr), any io.Writer supported
//
// - Ability to "mirror" screen output to log file or any other io.Writer
//
// - Screen and logfile targets can be independently managed, eg: screen
// gets normal output and errors, log gets full trace/debug output and is
// augmented with timestamps, Go file/line# for all log entry types
//
// - Does not insert carriage returns in output, does cleaner formatting of
// prefixes and meta-data with multiline or non-newline terminated output (vs
// Go's 'log' pkg)
//
// - Fatal errors can be marked up with a stack trace easily (via env or api)
//
// - Easily redirect or grab screen (or logfile) output, eg: via bufio io.Writer
//
// - Coming: github.com/dvln/in for prompting/paging to work w/this package
//
// Usage:   (Note: each is like 'fmt' syntax for Print, Printf, Println)
//	// For extremely detailed debugging, "<date/time> Trace: " prefix by default
//	out.Trace[f|ln](..)
//
//	// For basic debug output, "<date/time> Debug: " prefix by default to screen
//	out.Debug[f|ln](..)
//
//	// For user wants verbose but still "regular" output, no prefix to screen
//	out.Verbose[f|ln](..)
//
//	// For basic default "normal" output (typically), no prefix to screen
//	out.Print[f|ln](..)    |    out.Info[f|ln](..)   [both do same thing]
//
//	// For key notes for the user to consider (ideally), "Note: " prefix
//	out.Note[f|ln](..)
//
//	// For "expected" usage issues/errors (eg: bad flag value), "Issue: " prefix
//	out.Issue[f|ln](..)
//
//	// For system/setup class error, unexpected errors, "ERROR: " prefix
//	out.Error[f|ln](..)            (default screen out: os.Stderr)
//
//	// For fatal errors, will cause the tool to exit non-zero, "FATAL: " prefix
//	out.Fatal[f|ln](..)            (default screen out: os.Stderr)
//
// Note: logfile format defaults to: <date/time> <shortfile/line#> [Level: ]msg
//
// Aside: for my CLI's options I like "[-d | --debug]" and "[-v | --verbose]"
// to control tool output verbosity, ie: "-dv" (both) is the "output everything"
// level via the Trace level, "-d" sets the Debug level and all levels below,
// "-v" sets the Verbose level and all levels below and the default is the basic
// Info/Print level.  If the "[-q | --quiet]" opt one could further set that to
// map to the Issue level and perhaps "-qv" maps to the Note level.
//
package out

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// These bit flags are borrowed from Go's log package, behave the same "mostly"
// but handle multi-line strings and non-newline terminated strings better
// when adding markup like date/time and file/line# meta-data to either
// screen or log output stream (also better aligns the data overall).  If
// date and time with milliseconds is on and long filename w/line# it'll
// look like this:
//   2009/01/23 01:23:23.123123 /a/b/c/d.go:23: [LvlPrefix: ]<message>
// And with the flags not on (note that the level prefix depends upon what
// level one is printing output at and it can be adjusted as well):
//   [LvlPrefix: ]<message>
// See SetFlags() below for adjusting settings and Flags() to query settings
const (
	Ldate         = 1 << iota     // the date: 2009/01/23
	Ltime                         // the time: 01:23:23
	Lmicroseconds                 // microsecond resolution: 01:23:23.123123.  assumes Ltime.
	Llongfile                     // full file name path and line number: /a/b/c/d.go:23
	Lshortfile                    // just src file name and line #, eg: d.go:23. overrides Llongfile
	LstdFlags     = Ldate | Ltime // initial values for the standard logger
)

// Available output and logging levels to this package, by
// default "normal" info output and any notes/issues/errs/fatal/etc
// will be dumped to stdout and, by default, file logging for that output
// is inactive to start with til a log file is set up
const (
	LevelTrace   Level = iota // Very high amount of debug output
	LevelDebug                // Standard debug output level
	LevelVerbose              // Verbose output if user wants it
	LevelInfo                 // Standard output info/print to user
	LevelNote                 // Likely a heads up, "Note: <blah>"
	LevelIssue                // Typically a normal user/usage error
	LevelError                // Recoverable sys or unexpected error
	LevelFatal                // Very bad, we need to exit non-zero
	LevelDiscard              // Indicates no output at all if used
	// (writer set to ioutil.Discard also works)
	defaultScreenThreshold = LevelInfo    // Default out to regular info level
	defaultLogThreshold    = LevelDiscard // Default file logging starts off
)

// Some API's require an indication of if we're adjusting the "screen" output
// stream or the logfile output stream, use these bitflags anywhere you see
// forWhat arguments below.
const (
	ForScreen  = 1 << iota // Bit flag used to indicate screen target
	ForLogfile             // Used to indicate logfile target desired
)

// These are primarily for inserting prefixes on printed strings so we can put
// the prefix insert into different modes as needed, see doPrefixing() below.
const (
	alwaysInsert  = 1 << iota // Prefix every line, regardless of output history
	smartInsert               // Use previous output context to decide on prefix
	blankInsert               // Only spaces inserted (same length as prefix)
	skipFirstLine             // 1st line in multi-line string has no prefix
)

// Level type is just an int, see related const enum with LevelTrace, ..
type Level int

// An outputter represents an active output/log object that generates lines of
// output to one or two io.Writer(s) (typically screen which can be optionally
// mirrored to a log file when desired).  Each output operation makes a single
// call to each Writer's Write method.  An Outputter can be used simultaneously
// from multiple goroutines; it guarantees to serialize access to each Writer.
// Each log level (trace, debug, verbose, normal, note, issue, error, fatal)
// currently uses it's own io.Writer(s) and identifies any default prefixes,
// flags (if timestamp/filename/line# desired), etc.  These are bootstrapped
// at startup to reasonable values for screen output and can be controlled
// via the exported methods
type outputter struct {
	mu          sync.Mutex // ensures atomic writes; protects these fields:
	level       Level      // below data tells how this logging level works
	prefix      string     // prefix for this logging level (if any)
	buf         []byte     // for accumulating text to write at this level
	screenHndl  io.Writer  // io.Writer for "screen" output
	screenFlags int        // flags: additional metadata on screen output
	logfileHndl io.Writer  // io.Writer for "logfile" output
	logFlags    int        // flags: additional metadata on logfile output
}

var (
	// Set up each output level, ie: level, prefix, screen/log hndl, flags, ...
	trace   = &outputter{level: LevelTrace, prefix: "Trace: ", screenHndl: os.Stdout, screenFlags: LstdFlags, logfileHndl: ioutil.Discard, logFlags: LstdFlags | Lshortfile}
	debug   = &outputter{level: LevelDebug, prefix: "Debug: ", screenHndl: os.Stdout, screenFlags: LstdFlags, logfileHndl: ioutil.Discard, logFlags: LstdFlags | Lshortfile}
	verbose = &outputter{level: LevelVerbose, prefix: "", screenHndl: os.Stdout, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LstdFlags | Lshortfile}
	info    = &outputter{level: LevelInfo, prefix: "", screenHndl: os.Stdout, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LstdFlags | Lshortfile}
	note    = &outputter{level: LevelNote, prefix: "Note: ", screenHndl: os.Stdout, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LstdFlags | Lshortfile}
	issue   = &outputter{level: LevelIssue, prefix: "Issue: ", screenHndl: os.Stdout, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LstdFlags | Lshortfile}
	err     = &outputter{level: LevelError, prefix: "ERROR: ", screenHndl: os.Stderr, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LstdFlags | Lshortfile}
	fatal   = &outputter{level: LevelFatal, prefix: "FATAL: ", screenHndl: os.Stderr, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LstdFlags | Lshortfile}

	// Set up all the outputter level details in one array (except discard),
	// the idea that one can control these pretty flexibly (if needed)
	outputters = []*outputter{trace, debug, verbose, info, note, issue, err, fatal}

	// Set up default/starting logging threshold settings, see SetThreshold()
	// if you wish to change these threshold settings
	screenThreshold = defaultScreenThreshold
	logThreshold    = defaultLogThreshold
	logFileName     string

	// As output is displayed track if last message ended in a newline or not,
	// both to the screen and to the log (as levels may cause output to differ)
	// Note: this is tracked across *all* output levels so if you have done
	// something interesting like redirecting to different writers for logfile
	// output (eg: pointing at different log files) then this doesn't make a ton
	// of sense and you likely want to implement this ability via a couple of
	// booleans in the outputter struct and implement a mechanism to use that
	screenNewline  = true
	logfileNewline = true

	// used to request stack traces,on Fatal*, see SetStacktraceOnExit() below
	stacktraceOnExit = false
)

// levelCheck insures valid log level "values" are provided
func levelCheck(level Level) Level {
	switch {
	case level <= LevelTrace:
		return LevelTrace
	case level >= LevelDiscard:
		return LevelDiscard
	default:
		return level
	}
}

// Threshold returns the current screen or logfile output threshold level
// depending upon which is requested, either out.ForScreen or out.ForLogfile
func Threshold(forWhat int) Level {
	var threshold Level
	if forWhat&ForScreen != 0 {
		threshold = screenThreshold
	} else if forWhat&ForLogfile != 0 {
		threshold = logThreshold
	} else {
		Fatalln("Invalid screen/logfile given for Threshold()")
	}
	return threshold
}

// SetThreshold sets the screen and or logfile output threshold(s) to the given
// level, forWhat can be set to out.ForScreen, out.ForLogfile or both |'d
// together, level is out.LevelInfo for example (any valid level)
func SetThreshold(level Level, forWhat int) {
	if forWhat&ForScreen != 0 {
		screenThreshold = levelCheck(level)
	}
	if forWhat&ForLogfile != 0 {
		logThreshold = levelCheck(level)
	}
}

// SetPrefix sets screen and logfile output prefix to given string, note that
// it is recommended to have a trailing space on the prefix, eg: "Myprefix: "
// unless no prefix is desired then just "" will do
func SetPrefix(level Level, prefix string) {
	level = levelCheck(level)
	if level == LevelDiscard {
		return
	}
	// loop through the levels and reset the prefix of the specified level
	for _, o := range outputters {
		if o.level == level {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.prefix = prefix
		}
	}
}

// Discard disables all screen and/or logfile output, can be done via
// SetThreshold() as well (directly) or via SetWriter() to something
// like ioutil.Discard or bufio io.Writer if you want to capture output.
// Anyhow, this is a quick way to disable output (if forWhat is not set
// to out.ForScreen or out.ForLogfile or both | together nothing happens)
func Discard(forWhat int) {
	if forWhat&ForScreen != 0 {
		SetThreshold(LevelDiscard, ForScreen)
	}
	if forWhat&ForLogfile != 0 {
		SetThreshold(LevelDiscard, ForLogfile)
	}
}

// Flags gets the screen or logfile output flags (Ldate, Ltime, .. above),
// you must give one or the other (out.ForScreen or out.ForLogfile) only.
// FIXME: should take optional bitmap to indicate levels to set, else all (?)
func Flags(level Level, forWhat int) int {
	level = levelCheck(level)
	flags := 0
	for _, o := range outputters {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.level == level {
			if forWhat&ForScreen != 0 {
				flags = o.screenFlags
			} else if forWhat&ForLogfile != 0 {
				flags = o.logFlags
			} else {
				Fatalln("Invalid identification of screen or logfile target for Flags()")
			}
			break
		}
	}
	return (flags)
}

// SetFlags sets the screen and/or logfile output flags (Ldate, Ltime, .. above)
// Note: Right now this sets *every* levels log flags to given value, and one
// can give it out.ForScreen, out.ForLogfile or both or'd together although
// usually one would want to give just one to adjust (screen or logfile)
// FIXME: should take optional bitmap to indicate levels to set, else all (?)
func SetFlags(flags int, forWhat int) {
	for _, o := range outputters {
		o.mu.Lock()
		defer o.mu.Unlock()
		if forWhat&ForScreen != 0 {
			o.screenFlags = flags
		}
		if forWhat&ForLogfile != 0 {
			o.logFlags = flags
		}
	}
}

// Writer gets the screen or logfile output io.Writer for the given log
// level, forWhat is out.ForScreen or out.ForLogfile depending upon which
// writer you want to grab for the given logging level
func Writer(level Level, forWhat int) io.Writer {
	level = levelCheck(level)
	writer := ioutil.Discard
	for _, o := range outputters {
		if o.level == level {
			if forWhat&ForScreen != 0 {
				writer = o.screenHndl
			}
			if forWhat&ForLogfile != 0 {
				writer = o.logfileHndl
			}
		}
	}
	return (writer)
}

// SetWriter sets the screen and/or logfile output io.Writer for every log
// level to the given writer
// FIXME: should take optional bitmap to indicate levels to set, else all (?)
func SetWriter(w io.Writer, forWhat int) {
	for _, o := range outputters {
		o.mu.Lock()
		defer o.mu.Unlock()
		if forWhat&ForScreen != 0 {
			o.screenHndl = w
		}
		if forWhat&ForLogfile != 0 {
			o.logfileHndl = w
		}
	}
}

// ResetNewline allows one to reset the screen and/or logfile outputter so the
// next bit of output either "thinks" (or doesn't) that the previous output put
// the user on a new line.  If 'val' is true then the next output run through
// this pkg to the given output stream can be prefixed (with timestamps, etc),
// if it is false then no prefix, eg: out.Note("Enter data: ") might produce:
//   Note: enter data: <prompt>
// Which leaves the output stream thinking the last msg had no newline at the
// end of string.  Now, if one's input method reads input with the user hitting
// a newline then the below call can be used to tell the outputter(s) that a
// newline was hit and any fresh output can be prefixed cleanly:
//   out.ResetNewline(true, out.ForScreen|out.ForLogfile)
// Note: for any *output* running through this module this is auto-handled
func ResetNewline(val bool, forWhat int) {
	if forWhat&ForScreen != 0 {
		screenNewline = val
	}
	if forWhat&ForLogfile != 0 {
		logfileNewline = val
	}
}

// LogFileName returns any known log file name (if none returns "")
func LogFileName() string {
	return (logFileName)
}

// SetLogFile uses a log file path (passed in) to result in the log file
// output stream being targeted at this log file (and the log file created).
// Note: as to if anything is actually logged that depends upon the current
// logging level of course (default: LevelDiscard).  Please remember to set
// a log level to turn logging on, eg: SetLogThreshold(LevelInfo)
func SetLogFile(path string) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		Fatalln("Failed to open log file:", path, "Err:", err)
	}
	logFileName = file.Name()
	for _, o := range outputters {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.logfileHndl = file
	}
}

// UseTempLogFile creates a temp file and "points" the fileLogger logger at that
// temp file, the prefix passed in will be the start of the temp file name after
// which Go temp methods will generate the rest of the name, the temp file name
// will be returned as a string, errors will result in Fatalln()
// Note: to finish enabling logging remember to set the logging level to a valid
// level (LevelDiscard is the fileLog default), eg: SetLogThreshold(LevelInfo)
func UseTempLogFile(prefix string) string {
	file, err := ioutil.TempFile(os.TempDir(), prefix)
	if err != nil {
		Fatalln(err)
	}
	logFileName = file.Name()
	for _, o := range outputters {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.logfileHndl = file
	}
	return (logFileName)
}

// Next we head into the <Level>() class methods which don't add newlines
// and simply space separate the options sent to them:

// Trace is the most verbose debug level, space separate opts with no newline
// added and is by default prefixed with "Trace: <date/time> <msg>" for each
// line but you can use flags and remove the timestamp, can also drop the prefix
func Trace(v ...interface{}) {
	trace.output(v...)
}

// Debug is meant for basic debugging, space separate opts with no newline added
// and is, by default, prefixed with "Debug: <date/time> <your msg>" for each
// line but you can use flags and remove the timestamp, can also drop the prefix
func Debug(v ...interface{}) {
	debug.output(v...)
}

// Verbose meant for verbose user seen screen output, space separated
// opts printed with no newline added, no output prefix is added by default
func Verbose(v ...interface{}) {
	verbose.output(v...)
}

// Print is meant for "normal" user output, space separated opted
// printed with no newline added, no output prefix is added by default
func Print(v ...interface{}) {
	info.output(v...)
}

// Info is the same as Print: meant for "normal" user output, space separated
// opts printed with no newline added and no output prefix added by default
func Info(v ...interface{}) {
	info.output(v...)
}

// Note is meant for output of key "note" the user should pay attention to, opts
// space separated and printed with no newline added, "Note: <msg>" prefix is
// also added by default
func Note(v ...interface{}) {
	note.output(v...)
}

// Issue is meant for "normal" user error output, space separated opts
// printed with no newline added, "Error: <msg>" prefix added by default
func Issue(v ...interface{}) {
	issue.output(v...)
}

// Error is meant for "unexpected"/system error output, space separated
// opts printed with no newline added, "ERROR: <msg>" prefix added by default
// Note: by "unexpected" these are things like filesystem permissions
// problems, see Note/Issue for more normal user level usage issues
func Error(v ...interface{}) {
	err.output(v...)
}

// Fatal is meant for "unexpected"/system fatal error output, space separated
// opts printed with no newline added, "FATAL: <msg>" prefix added by default
// and the tool will exit non-zero here
func Fatal(v ...interface{}) {
	fatal.output(v...)
}

// Next we head into the <Level>ln() class methods which add newlines
// and space separate the options sent to them:

// Traceln is the most verbose debug level, space separate opts with newline
// added and is, by default, prefixed with "Trace: <your output>" for each line
// but you can use flags and remove the timestamp, can also drop the prefix
func Traceln(v ...interface{}) {
	trace.outputln(v...)
}

// Debugln is meant for basic debugging, space separate opts with newline added
// and is, by default, prefixed with "Debug: <date/time> <yourmsg>" for each
// line but you can use flags and remove the timestamp, can also drop the prefix
func Debugln(v ...interface{}) {
	debug.outputln(v...)
}

// Verboseln is meant for verbose user seen screen output, space separated
// opts printed with newline added, no output prefix is added by default
func Verboseln(v ...interface{}) {
	verbose.outputln(v...)
}

// Println is the same as Infoln: meant for "normal" user output, space
// separated opts printed with newline added and no output prefix added by
// default
func Println(v ...interface{}) {
	info.outputln(v...)
}

// Infoln is the same as Println: meant for "normal" user output, space
// separated opts printed with newline added and no output prefix added by
// default
func Infoln(v ...interface{}) {
	info.outputln(v...)
}

// Noteln is meant for output of key items the user should pay attention to,
// opts are space separated and printed with a newline added, "Note: <msg>"
// prefix is also added by default
func Noteln(v ...interface{}) {
	note.outputln(v...)
}

// Issueln is meant for "normal" user error output, space separated
// opts printed with no newline added, "Issue: <msg>" prefix added by default
// Note: by "normal" these are things like unknown codebase name given, etc...
// for unexpected errors use Errorln (eg: file system full, etc)
func Issueln(v ...interface{}) {
	issue.outputln(v...)
}

// Errorln is meant for "unexpected"/system error output, space separated
// opts printed with no newline added, "ERROR: <msg>" prefix added by default
// Note: by "unexpected" these are things like filesystem permissions problems,
// see Noteln/Issueln for more normal user level notes/usage
func Errorln(v ...interface{}) {
	err.outputln(v...)
}

// Fatalln is meant for "unexpected"/system fatal error output, space separated
// opts printed with no newline added, "FATAL: <msg>" prefix added by default
// and the tool will exit non-zero here.  Note that a stacktrace can be added
// for fatal errors, see PKG_OUT_NONZERO_EXIT_STACKTRACE
func Fatalln(v ...interface{}) {
	fatal.outputln(v...)
}

// Next we head into the <Level>f() class methods which take a standard
// format string for go (see 'godoc fmt' and look at Printf() if needed):

// Tracef is the most verbose debug level, format string followed by args and
// output is, by default, prefixed with "Trace: <date/time> <your msg>" for each
// line but you can use flags and remove the timestamp, can also drop the prefix
func Tracef(format string, v ...interface{}) {
	trace.outputf(format, v...)
}

// Debugf is meant for basic debugging, format string followed by args and
// output is by default prefixed with "Debug: <date/time> <your msg>" for each
// line but you can use flags and remove the timestamp, can also drop the prefix
func Debugf(format string, v ...interface{}) {
	debug.outputf(format, v...)
}

// Verbosef is meant for verbose user seen screen output, format string
// followed by args (and no output prefix is added by default)
func Verbosef(format string, v ...interface{}) {
	verbose.outputf(format, v...)
}

// Printf is the same as Infoln: meant for "normal" user output, format string
// followed by args (and no output prefix added by default)
func Printf(format string, v ...interface{}) {
	info.outputf(format, v...)
}

// Infof is the same as Printf: meant for "normal" user output, format string
// followed by args (and no output prefix added by default)
func Infof(format string, v ...interface{}) {
	info.outputf(format, v...)
}

// Notef is meant for output of key "note" the user should pay attention to,
// format string followed by args, "Note: <yourmsg>" prefixed by default
func Notef(format string, v ...interface{}) {
	note.outputf(format, v...)
}

// Issuef is meant for "normal" user error output, format string followed
// by args, prefix "Issue: <msg>" added by default
func Issuef(format string, v ...interface{}) {
	issue.outputf(format, v...)
}

// Errorf is meant for "unexpected"/system error output, format string
// followed by args, prefix "ERROR: <msg>" added by default
// Note: by "unexpected" these are things like filesystem permissions problems,
// see Notef/Issuef for more normal user level notes/usage
func Errorf(format string, v ...interface{}) {
	err.outputf(format, v...)
}

// Fatalf is meant for "unexpected"/system fatal error output, format string
// followed by args, prefix "FATAL: <msg>" added by default and will exit
// non-zero from the tool (see Go 'log' Fatalf() method)
func Fatalf(format string, v ...interface{}) {
	fatal.outputf(format, v...)
}

// SetStacktraceOnExit can be used to flip on stack traces programatically, one
// can also use PKG_OUT_NONZERO_EXIT_STACKTRACE set to "1" as another way, this
// is meant for Fatal[f|ln]() class output/exit
func SetStacktraceOnExit(val bool) {
	stacktraceOnExit = val
}

// getStackTrace will get a stack trace (truncated at 4096 bytes currently)
// if and only if PKG_OUT_NONZERO_EXIT_STACKTRACE is set to "1"
func getStackTrace() string {
	var myStack string
	if stacktraceOnExit || os.Getenv("PKG_OUT_NONZERO_EXIT_STACKTRACE") == "1" {
		trace := make([]byte, 4096)
		count := runtime.Stack(trace, true)
		// trace will be populated with NUL chars for anything not filled in,
		// Go isn't C so we need to trim those out otherwise they'll be printed
		trace = bytes.Trim(trace, "\x00")
		myStack = fmt.Sprintf("stacktrace: stack of %d bytes:\n%s\n", count, trace)
	}
	return myStack
}

// insertPrefix takes a multiline string (potentially) and for each
// line places a string prefix in front of each line, for control
// there is a bitmap of:
//   alwaysInsert            // Prefix every line, regardless of output history
//   smartInsert             // Use previous output context to decide on prefix
//   blankInsert             // Only spaces inserted (same length as prefix)
//   skipFirstLine           // 1st line in multi-line string has no prefix
func insertPrefix(s string, prefix string, ctrl int) string {
	if prefix == "" {
		return s
	}
	if ctrl&alwaysInsert != 0 {
		ctrl = 0 // turn off everything, always means *always*
	}
	pfxLength := len(prefix)
	lines := strings.Split(s, "\n")
	numLines := len(lines)
	newLines := []string{}
	for idx, line := range lines {
		if (idx == numLines-1 && line == "") ||
			(idx == 0 && ctrl&skipFirstLine != 0) {
			newLines = append(newLines, line)
		} else if ctrl&blankInsert != 0 {
			format := "%" + fmt.Sprintf("%d", pfxLength) + "s"
			spacePrefix := fmt.Sprintf(format, "")
			newLines = append(newLines, spacePrefix+line)
		} else {
			newLines = append(newLines, prefix+line)
		}
	}
	newstr := strings.Join(newLines, "\n")
	return newstr
}

// output is similar to fmt.Print(), it'll space separate args with no newline
// and output them to the screen and/or log file loggers based on levels
func (o *outputter) output(v ...interface{}) {
	// set up the message to dump
	msg := fmt.Sprint(v...)
	// dump it based on screen and log output levels
	o.stringOutput(msg)
}

// outputln is similar to fmt.Println(), it'll space separate args with no newline
// and output them to the screen and/or log file loggers based on levels
func (o *outputter) outputln(v ...interface{}) {
	// set up the message to dump
	msg := fmt.Sprintln(v...)
	// dump it based on screen and log output levels
	o.stringOutput(msg)
}

// outputf is similar to fmt.Printf(), it takes a format and args and outputs
// the resulting string to the screen and/or log file loggers based on levels
func (o *outputter) outputf(format string, v ...interface{}) {
	// set up the message to dump
	msg := fmt.Sprintf(format, v...)
	// dump it based on screen and log output levels
	o.stringOutput(msg)
}

// iota converts an int to fixed-width decimal ASCII.  Give a negative width to
// avoid zero-padding.  Knows the buffer has capacity.  Taken from Go's 'log'
// pkg since we want the same formatting but we want to indent multi-line
// strings a bit more cleanly for readability in logs and on output.
func itoa(buf *[]byte, i int, wid int) {
	u := uint(i)
	if u == 0 && wid <= 1 {
		*buf = append(*buf, '0')
		return
	}
	// Assemble decimal in reverse order.
	var b [32]byte
	bp := len(b)
	for ; u > 0 || wid > 0; u /= 10 {
		bp--
		wid--
		b[bp] = byte(u%10) + '0'
	}
	*buf = append(*buf, b[bp:]...)
}

// getFlagString takes the time the output func was called and tries
// to construct a Go 'log' type set of settings (actually using the
// flag bitmap from log and borrowing the logic from log.go)
func getFlagString(buf *[]byte, flags int, file string, line int, t time.Time) string {
	if flags&(Ldate|Ltime|Lmicroseconds) != 0 {
		if flags&Ldate != 0 {
			year, month, day := t.Date()
			itoa(buf, year, 4)
			*buf = append(*buf, '/')
			itoa(buf, int(month), 2)
			*buf = append(*buf, '/')
			itoa(buf, day, 2)
			*buf = append(*buf, ' ')
		}
		if flags&(Ltime|Lmicroseconds) != 0 {
			hour, min, sec := t.Clock()
			itoa(buf, hour, 2)
			*buf = append(*buf, ':')
			itoa(buf, min, 2)
			*buf = append(*buf, ':')
			itoa(buf, sec, 2)
			if flags&Lmicroseconds != 0 {
				*buf = append(*buf, '.')
				itoa(buf, t.Nanosecond()/1e3, 6)
			}
			*buf = append(*buf, ' ')
		}
	}
	if flags&(Lshortfile|Llongfile) != 0 {
		if flags&Lshortfile != 0 {
			short := file
			for i := len(file) - 1; i > 0; i-- {
				if file[i] == '/' {
					short = file[i+1:]
					break
				}
			}
			file = short
			file = fmt.Sprintf("%16s", file)
		} else {
			file = fmt.Sprintf("%40s", file)
		}
		*buf = append(*buf, file...)
		*buf = append(*buf, ':')
		itoa(buf, line, -1)
		*buf = append(*buf, ": "...)
	}
	return fmt.Sprintf("%s", *buf)
}

// insertFlagMetadata basically checks to see what flags are set for
// the current screen or logfile output and inserts the meta-data in
// front of the string, see insertPrefix for ctrl description, tgt
// here is either ForScreen or ForLogfile (constants) for output
func (o *outputter) insertFlagMetadata(s string, tgt int, ctrl int) string {
	now := time.Now() // do this before Caller below, can take some time
	var file string
	var line int
	o.mu.Lock()
	defer o.mu.Unlock()
	var flags int
	// if printing to the screen target use those flags, else use logfile flags
	if tgt&ForScreen != 0 {
		flags = o.screenFlags
	} else if tgt&ForLogfile != 0 {
		flags = o.logFlags
	} else {
		Fatalln("Invalid target passed to insertFlagMetadata():", tgt)
	}
	if flags&(Lshortfile|Llongfile) != 0 {
		// this can take a little while so unlock the mutex
		o.mu.Unlock()
		var ok bool
		_, file, line, ok = runtime.Caller(5)
		if !ok {
			file = "???"
			line = 0
		}
		o.mu.Lock()
	}
	o.buf = o.buf[:0]
	leader := getFlagString(&o.buf, flags, file, line, now)
	if leader == "" {
		return s
	}
	s = insertPrefix(s, leader, ctrl)
	return (s)
}

// doPrefixing takes the users output string and decides how to prefix
// the users message based on the log level and any associated prefix,
// eg: "Debug: ", as well as any flag settings that could add date/time
// and information on the calling Go file and line# and such.
//
// An example of what prefixing means might be useful here, if our code has:
//   [13:]  out.Noteln("This is a test\n", "and only a test\n")
//   [14:]  out.Noteln("that I am showing to ")
//   [15:]  out.Notef("%s\n", getUserName())
//   [16:]  out.Noteln("...")
// It would result in output like so to the screen (typically, flags to adjust):
//   Note: This is a test
//   Note: and only a test
//   Note: that I am showing to John
// Aside: other levels like Debug and Trace add in date/time to screen output
// Log file entry and formatting for the same code if logging is active:
//   <date/time> myfile.go:13: Note: This is a test
//   <date/time> myfile.go:13: Note: and only a test
//   <date/time> myfile.go:14: Note: that I am showing to John
//
// The only thing we "lose" here potentially is that the line that prints
// the username isn't be prefixed to keep the output clean (no line #15 details)
// hence we don't have a date/timestamp for that "part" of the output and that
// could cause someone to think it was line 14 that was slow if the next entry
// was 20 minutes later (eg: the myfile.go line 16 print statement).  There is
// a mode to turn off smart flags prefixing so you can see that, one would set
// PKG_OUT_SMART_FLAGS_PREFIX to "off" to cause this (since no date/time or
// other flags are active in the prefix for screen output it remains the same):
//   Note: This is a test
//   Note: and only a test
//   Note: and that is it John
// The log file entry differs though as we can see myfile:15 detail now:
//   <date/time> myfile.go:13: Note: This is a test
//   <date/time> myfile.go:13: Note: and only a test
//   <date/time> myfile.go:14: Note: that I am showing to <date/time> myfile:15: John
// Obviously makes the output uglier but might be of use now and then.
//
// One more note, if a stack trace is added on a Fatal error (if turned on)
// then we force add a newline if the fatal doesn't have one and dump the
// stack trace with 'blankInsert' so the stack trace is associated with
// that fatal print, eg:
//   os.Setenv("PKG_OUT_NONZERO_EXIT_STACKTRACE", "1")
//   out.Fatal("Severe error, giving up\n")    [use better errors of course]
// Screen output:
//   FATAL: Severe error, giving up
//   FATAL: <multiline stacktrace here>
// Log file entry:
//   <date/time> myfile.go:37: FATAL: Severe error, giving up
//   <date/time> myfile.go:37: FATAL: <multiline stacktrace here>
// The goal being readability of the screen and logfile output while conveying
// information about date/time and source of the fatal error and such
func (o *outputter) doPrefixing(s string, forWhat int, ctrl int) string {
	// where we check out if we previously had no newline and if so the
	// first line (if multiline) will not have the prefix, see example
	// in function header around username
	var onNewline bool
	if forWhat&ForScreen != 0 {
		onNewline = screenNewline
	} else if forWhat&ForLogfile != 0 {
		onNewline = logfileNewline
	} else {
		Fatalln("Invalid target for output given in doPrefixing():", forWhat)
	}
	if !onNewline && ctrl&smartInsert != 0 {
		ctrl = ctrl | skipFirstLine
	}
	s = insertPrefix(s, o.prefix, ctrl)

	if os.Getenv("PKG_OUT_SMART_FLAGS_PREFIX") == "off" {
		ctrl = alwaysInsert // forcibly add prefix without smarts
	}
	// now set up metadata prefix (eg: timestamp), if any, same as above
	// it has the brains to not add in a prefix if not needed or wanted
	s = o.insertFlagMetadata(s, forWhat, ctrl)
	return (s)
}

// stringOutput uses existing screen and log levels to decide what, if
// anything, is printed to the screen and/or log file Writer(s) based on
// current screen and log output thresholds, flags and stack trace settings
func (o *outputter) stringOutput(s string) {
	// print to the screen output writer first
	var stacktrace string
	if o.level == LevelFatal {
		stacktrace = getStackTrace()
	}
	if o.level >= screenThreshold && o.level != LevelDiscard {
		pfxScreenStr := o.doPrefixing(s, ForScreen, smartInsert)
		_, err := o.screenHndl.Write([]byte(pfxScreenStr))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%sError writing to screen output handler:\n%+v\noutput:\n%s\n", o.prefix, err, s)
			os.Exit(1)
		}
		if s[len(s)-1] == 0x0A { // if last char is a newline..
			screenNewline = true
		} else {
			screenNewline = false
		}
		if o.level == LevelFatal {
			if !screenNewline {
				// ignore errors, just quick "prettyup" attempt:
				o.screenHndl.Write([]byte("\n"))
			}
			_, err = o.screenHndl.Write([]byte(o.doPrefixing(stacktrace, ForScreen, smartInsert)))
			if err != nil {
				fmt.Fprintf(os.Stderr, "%sError writing stacktrace to screen output handle:\n%+v\n", o.prefix, err)
				os.Exit(1)
			}
		}
	}

	// print to the log file writer next
	if o.level >= logThreshold && o.level != LevelDiscard {
		pfxLogfileStr := o.doPrefixing(s, ForLogfile, smartInsert)
		o.logfileHndl.Write([]byte(pfxLogfileStr))
		if s[len(s)-1] == 0x0A {
			logfileNewline = true
		} else {
			logfileNewline = false
		}
		if o.level == LevelFatal {
			if !logfileNewline {
				o.logfileHndl.Write([]byte("\n"))
			}
			o.logfileHndl.Write([]byte(o.doPrefixing(stacktrace, ForLogfile, smartInsert)))
		}
	}
	// if we're fatal erroring then we need to exit unless overrides in play,
	// this env var should be used for test suites only really...
	if o.level == LevelFatal &&
		os.Getenv("PKG_OUT_NO_EXIT") != "1" {
		os.Exit(1)
	}
}
