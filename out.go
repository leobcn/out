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

// Package out is for easy and flexible CLI output and log handling.  It has
// taken ideas from the Go author log package, spf13's jwalterweatherman package,
// Dropbox errors package and other packages out there (many thanks to all the
// talented folks!!!).  Goals of this pkg:
//
// - Leveled output: trace, debug, verbose, print, note, issue, error, fatal
//
// - Works out of box for screen output (stdout/stderr), any io.Writer supported
//
// - Trivial to "mirror" screen output to log file (any io.Writer, eg: buffer)
//
// - Ability to dynamically filter debugging output by function or pkg path
//
// - Screen and logfile targets can be independently managed, eg: screen
// gets normal output and errors, log gets full trace/debug output and is
// augmented with timestamps, Go file/line# for all log entry types, etc
//
// - Does not insert carriage returns in output, does clean formatting of
// prefixes and meta-data with multiline or non-newline terminated output (vs
// Go's 'log' pkg which uses newlines after all output)
//
// - Stack traces on issues/errors/fatals easily available (via env or api)
//
// - Extended errors with "closer to the issue" stack tracing, error codes
// (optional/extensible), error "stacking/wrapping" still providing easy
// "constant" error matching matching from Go stdlib packages
//
// - Custom formatter: if existing formatting options are not enough, can
// be used to produce custom output formats or even do things like suppress
// output while pushing that into some other mechanism (eg: if in JSON output
// mode push output into the pkg forming up the JSON response so errors show
// up in JSON output or warnings are stored there, etc)
//
// - Ability to set up a "deferred" function that will be called just before
// os.Exit is called (assumes you are exiting via out.Exit(), out.Fatal() and
// related out mechanisms for exitting), receives exit value for tool.  Eg: use
// this when client uses a tool generated tmp logfile name to record the tools
// output... so the tmp logfile name is printed at the bottom of their output
// just before exit and so the note is sent only to the screen on STDERR.
//
// - Future: github.com/dvln/in for prompting/paging
//
// The 'out' package is designed as a singleton (currently) although one could
// make it more generic... but as I have no need for that currently I've avoided
// that effort.  If done maybe group []*LvlOutput in an "Outputter" struct, add
// methods for all desired functions like 'out.Print()' on (*Outputter) and move
// the logic into that and have the singleton function call these methods.  Then
// perhaps clean up the *Newline stuff (should be done anyhow) so it drives off
// the io.Writers targets (consider os.Stdout and os.Stderr to be the same tgt
// no matter how many writers point at it, and consider any other io.Writer
// like a file or a buffer to be the same if the same "handle"... anyhow, needs
// to be better than what's here now).  What could go wrong?  ;)
//
// Anyhow, for true screen mirroring to logfile type controls it's pretty
// effective as a singleton so have some fun.  Also, as a more powerful error
// mechanism (wrapped/nested errors with stack traces near the source of the
// err available, still easy to check constant err values, optional ability
// to add err codes if desired) it can be of use.
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
//	// For system/setup class error, unexpected errors, "Error: " prefix
//	out.Error[f|ln](..)            (default screen out: os.Stderr)
//
//	// For fatal errors, will cause the tool to exit non-zero, "Fatal: " prefix
//	out.Fatal[f|ln](..)            (default screen out: os.Stderr)
//
// Note: logfile format defaults to: <pid> <LEVEL> <date/time> <shortfile/line#> [Level: ]msg
//
// Aside: for CLI tool options I like "[-D|--debug]" and "[-v|--verbose]"
// to control tool output verbosity, ie: "-Dv" (both) is the "output everything"
// mode via the Trace level, just "-D" is the Debug level and all levels below,
// only "-v" sets the Verbose level and all levels below and Info/Print is the
// default level with none of those options.
//
// Quick Plug: I like spf13's viper&cobra pkgs for CLI and config file mgmt
package out

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// mutex is used for writing to global vars and Writing to what might
// be stdout and stderr (or may not be)... try to be goroutine friendly
var mutex sync.RWMutex

// Some of these flags are borrowed from Go's log package and "mostly" behave
// the same but handle multi-line strings and non-newline terminated strings
// differently when adding markup like date/time and file/line# meta-data to
// either screen or log output stream (also better aligns the data overall).
// If date and time with milliseconds is on and long filename w/line# it'll
// look like this:
//   2009/01/23 01:23:23.123123 /a/b/c/d.go:23: [LvlPrefix: ]<mesg>
// If one adds in the pid and level settings it will look like this:
//   [pid] LEVEL 2009/01/23 01:23:23.123123 /a/b/c/d.go:23: [LvlPrefix: ]<mesg>
// And with the flags not on (note that the level prefix depends upon what
// level one is printing output at and it can be adjusted as well):
//   [LvlPrefix: ]<message>
// See SetFlags() below for adjusting settings and Flags() to query settings.
const (
	Ldate         = 1 << iota             // the date: 2009/01/23
	Ltime                                 // the time: 01:23:23
	Lmicroseconds                         // microsecond resolution: 01:23:23.123123.  assumes Ltime.
	Llongfile                             // full file name path and line number: /a/b/c/d.go:23
	Lshortfile                            // just src file name and line #, eg: d.go:23. overrides Llongfile
	Llongfunc                             // full func signature, dvln/cmd.get for get method in dvln/cmd
	Lshortfunc                            // just short func signature, trimmed to just get
	Lpid                                  // add in the pid to the output
	Llevel                                // add in the output level "raw" string (eg: TRACE,DEBUG,..)
	LstdFlags     = Ldate | Ltime         // for those used to Go 'log' flag settings
	LscreenFlags  = Ltime | Lmicroseconds // values for "std" screen and log file flags
	LlogfileFlags = Lpid | Llevel | Ldate | Ltime | Lmicroseconds | Lshortfile | Lshortfunc
)

// FEATURE: add in userid to the list of goodies, turn on by default for logfile
// so it looks like this:
// [616]   brady INFO    2015/07/25 01:05:01.886736 get.go:75:get                 : Look up codebase
// FEATURE: clean up the out.go file a bit:
// - migrate detailed error stuff "mostly" into deterr.go module in this dir
// - migrate formatter stuff "mostly" into fmt.go module in this dir

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
	LevelAll                  // Used for a few API's to indicate set all levels
	// (writer set to ioutil.Discard also works)
	defaultScreenThreshold = LevelInfo    // Default out to regular info level
	defaultLogThreshold    = LevelDiscard // Default file logging starts off
)

// Some API's require "flags" to identify if the API effects the screen output
// stream or the logfile output stream, or both.  Use these flags as needed
// to indicate what to check/control (screen, logfile or both).  There are
// also a couple flags to control stack tracing in issues/errors.  One can
// identify that, if stack trace erroring is active, if it happens for all
// issues/errors or if only warn/error *with* exit situations (these stack
// trace settings can be combined with the ForScreen/ForLogfile/ForBoth
// flags to indicate which targets gets the stack traces printed, the
// default setting is ForLogfile combined with StackNonZeroErrorExit):
const (
	ForScreen                  = 1 << iota              // Check/control screen output target
	ForLogfile                                          // Check/control logfile output target
	StackTraceNonZeroErrorExit                          // Indicate if stacktrace used on exit
	StackTraceErrorExit                                 // Indicate if stacktrace used on exit
	StackTraceAllIssues                                 // If stacktrace dumped on issues/errs
	ForBoth                    = ForScreen | ForLogfile // Indicate both screen/logfile targets
	// StackTraceExitToLogfile is an alias for the starting stack trace config
	StackTraceExitToLogfile = StackTraceNonZeroErrorExit | ForLogfile
)

// These are primarily for inserting prefixes on printed strings so we can put
// the prefix insert into different modes as needed, see doPrefixing() below.
const (
	AlwaysInsert  = 1 << iota // Prefix every line, regardless of output history
	SmartInsert               // Output context "til now" decides if prefix used
	BlankInsert               // Only spaces inserted (same length as prefix)
	SkipFirstLine             // 1st line in multi-line string has no prefix
)

// Level type is just an int, see related const enum with LevelTrace, ..
type Level int

// LvlOutput structures define io.Writers (eg: one for screen, one for log) to
// flexibly control outputting to a given output "level".  Each writer has a
// set of flags associated indicating what augmentation the output might have
// and there is a single, optional, prefix that will be inserted before any
// message to that level (regardless of screen or logfile).  There are 8 levels
// defined and placed into a array of LvlOutput pointers, []outputters.  Each
// levels output struct screen and log file writers can be individually
// controlled (but would typically all point to stdout/stderr for the screen
// target and the same log file writer or buffer writer for all logfile writers
// for each level... but don't have to).  The log file levels provided are
// currently: trace, debug, verbose, normal, note, issue, error and fatal
// which map to the related singleton functions of the same name, ie:
// Trace[f|ln](), Debug[f|ln](), Verbose[f|ln](), etc.  All prefixes and
// screen handles and such are "bootstrapped" below and can be controlled
// via various methods to change writers, prefixes, overall threshold levels
// and newline tracking, etc.  Aside: below there is a also an io.Writer that
// corresponds to each level, ie: fmt.Fprintf(TRACE, "%s", someStr), as a 2nd
// way to push output through the screen/log writers that are set up.
// Note: should create a LvlOutputter interface one of these days, no?
type LvlOutput struct {
	mu          sync.RWMutex // ensures atomic writes; protects these fields:
	level       Level        // below data tells how each logging level works
	prefix      string       // prefix for this logging level (if any)
	buf         []byte       // for accumulating text to write at this level
	screenHndl  io.Writer    // io.Writer for "screen" output
	screenFlags int          // flags: additional metadata on screen output
	logfileHndl io.Writer    // io.Writer for "logfile" output
	logFlags    int          // flags: additional metadata on logfile output
	formatter   Formatter    // optional output formatting extension/plugin
}

// FlagMetadata stores the various log add-on fields that a client can request
// such as a timestamp, the log level, the package, routine and line number
// information, pid, etc
type FlagMetadata struct {
	Time   *time.Time `json:"time,omitempty"`
	Path   string     `json:"path,omitempty"`
	File   string     `json:"file,omitempty"`
	Func   string     `json:"func,omitempty"`
	LineNo int        `json:"lineno,omitempty"`
	Level  string     `json:"level,omitempty"`
	PID    int        `json:"pid,omitempty"`
	Stack  string     `json:"stack,omitempty"`
}

var (
	// Set up each output level, ie: level, prefix, screen/log hndl, flags, ...

	// TRACE can be used as an io.Writer for trace level output
	TRACE = &LvlOutput{level: LevelTrace, prefix: "Trace: ", screenHndl: os.Stdout, screenFlags: LscreenFlags, logfileHndl: ioutil.Discard, logFlags: LlogfileFlags}
	// DEBUG can be used as an io.Writer for debug level output
	DEBUG = &LvlOutput{level: LevelDebug, prefix: "Debug: ", screenHndl: os.Stdout, screenFlags: LscreenFlags, logfileHndl: ioutil.Discard, logFlags: LlogfileFlags}
	// VERBOSE can be used as an io.Writer for verbose level output
	VERBOSE = &LvlOutput{level: LevelVerbose, prefix: "", screenHndl: os.Stdout, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LlogfileFlags}
	// INFO can be used as an io.Writer for info|print level output
	INFO = &LvlOutput{level: LevelInfo, prefix: "", screenHndl: os.Stdout, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LlogfileFlags}
	// NOTE can be used as an io.Writer for note level output
	NOTE = &LvlOutput{level: LevelNote, prefix: "Note: ", screenHndl: os.Stdout, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LlogfileFlags}
	// ISSUE can be used as an io.Writer for issue level output
	ISSUE = &LvlOutput{level: LevelIssue, prefix: "Issue: ", screenHndl: os.Stdout, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LlogfileFlags}
	// ERROR can be used as an io.Writer for error level output
	ERROR = &LvlOutput{level: LevelError, prefix: "Error: ", screenHndl: os.Stderr, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LlogfileFlags}
	// FATAL can be used as an io.Writer for fatal level output
	FATAL = &LvlOutput{level: LevelFatal, prefix: "Fatal: ", screenHndl: os.Stderr, screenFlags: 0, logfileHndl: ioutil.Discard, logFlags: LlogfileFlags}

	// Set up all the LvlOutput level details in one array (except discard),
	// the idea that one can control these pretty flexibly (if needed)
	outputters = []*LvlOutput{TRACE, DEBUG, VERBOSE, INFO, NOTE, ISSUE, ERROR, FATAL}

	// Set up default/starting logging threshold settings, see SetThreshold()
	// if you wish to change these threshold settings
	screenThreshold = defaultScreenThreshold
	logThreshold    = defaultLogThreshold
	logFileName     string

	// As output is displayed track if last message ended in a newline or not,
	// both to the screen and to the log (as levels may cause output to differ)
	// Note: this is tracked across *all* output levels so if you have done
	// something "interesting" like redirecting to different writers for logfile
	// output (eg: pointing at different log files for different levels) then
	// the below globs don't really work since they treat screen output (all
	// levels as visible in the same "stream" and log output the same way).
	// If you're doing this then you may need to re-work the package a bit,
	// you could track *NewLines for each level independently for example.
	screenNewline  = true
	logfileNewline = true

	// stackTraceConfig is used to ask for stack traces to be dumped on various
	// classes of errors (or issues), the default is to dump stack traces to
	// the logfile output stream on error/exit (assuming the 'out' package is
	// being used for that non-zero exit process via Fatal, Exit(<non-zero>),
	// ErrorExit or IssueExit).  See SetStackTraceConfig() to change.
	stackTraceConfig = StackTraceExitToLogfile

	// The below "<..>NameLength" flags help to aligh the output when dumping
	// filenames, line #'s' and function names to a log file in front of the
	// tools normal output.  This is weak (at best), but usually works "ok"
	// for paths, file and func name lengths that tend towards "short".  Note
	// that if you have different log levels to the same output stream using
	// different combos of filename/line# and func name meta-data then your
	// output won't align well (currently), opted not to get too fancy now.

	// shortFileNameLength is the default "formatting" length for file/line#
	// from runtime.Caller() (just the filename part of the path), right now
	// we'll hope filenames don't usually get longer than 10 chars or so (and
	// there is the :<line#> part of the block which is around 5 chars and
	// then the trailing colon, so we'll go with 16).  If you have longer
	// filenames then you can change this setting so your output alignment
	// improves (or the below settings)
	shortFileNameLength int32 = 16

	// longFileNameLength is the full path and filename plus the line # and
	// a trailing colon after that... this is hand-wavy but we'll give it
	// some space for now, adjust as needed for your paths/filenames:
	longFileNameLength int32 = 55

	// shortFuncNameLength ties into function names (if those have been added
	// to your output metadata via the Lshortfunc flag), right now it expects
	// method names of around 12 or 13 chars, followed by a colon, adjust as
	// needed for your own method names
	shortFuncNameLength int32 = 14

	// longFuncNameLength is the full function name which includes the package
	// name (full path) followed by a dot and then the function name, this may
	// be a bit short for some folks so adjust as needed.
	longFuncNameLength int32 = 30

	// callDepth is for runtime.Caller() to identify where a Noteln() or Print()
	// or Issuef() (etc) was called from (so meta-data dumped in "extended"
	// mode gives the correct calling function and line number).  The existing
	// value is correct *but* if you choose to further wrap 'out' methods in
	// some extra method layer (or two) in your own modules then you might
	// want to increase it via this public package global.
	callDepth int32 = 5

	// errorExitVal is the default exit value used by Fatal()* routines which
	// are not given an exit value to use
	errorExitVal int32 = -1

	// deferFunc is a func pointer to a func that takes no params and returns
	// nothing of use, if set it is called immediately before exit (often used
	// for printing final messages with stat's/timing or perhaps a note on
	// a temp output logfile name so it's visible at the end of a run, etc),
	// See DeferFunc() and SetDeferFunc() to get and set this if desired.
	deferFunc func(exitVal int)
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

// DeferFunc returns a function type (reference type) if a defer func has
// been set, see SetDeferFunc(), otherwise nil.  A defer function is one that
// is fired right before os.Exit() is called by the'out' package.
// WARNING: this is not goroutine safe, don't use this in goroutines
func DeferFunc() func(exitVal int) {
	return deferFunc
}

// SetDeferFunc sets a single deferred funtion that is called right before
// the out package exits (if one is configured).  The function reference
// passed in should have a signature of exitVal (int) coming in and nothing
// being returned.
func SetDeferFunc(dFunc func(exitVal int)) {
	// Safely adjust the messenger func
	mutex.Lock()
	{
		deferFunc = dFunc
	}
	mutex.Unlock()
}

// Threshold returns the current screen or logfile output threshold level
// depending upon which is requested, either out.ForScreen or out.ForLogfile
func Threshold(outputTgt int) Level {
	mutex.Lock()
	defer mutex.Unlock()
	var threshold Level
	if outputTgt&ForScreen != 0 {
		threshold = screenThreshold
	} else if outputTgt&ForLogfile != 0 {
		threshold = logThreshold
	} else {
		Fatalln("Invalid screen/logfile given for Threshold()")
	}
	return threshold
}

// SetThreshold sets the screen and or logfile output threshold(s) to the given
// level, outputTgt can be set to out.ForScreen, out.ForLogfile or both |'d
// together, level is out.LevelInfo for example (any valid level)
func SetThreshold(level Level, outputTgt int) {
	if outputTgt&ForScreen != 0 {
		lc := levelCheck(level)
		mutex.Lock()
		screenThreshold = lc
		mutex.Unlock()
	}
	if outputTgt&ForLogfile != 0 {
		lc := levelCheck(level)
		mutex.Lock()
		logThreshold = lc
		mutex.Unlock()
	}
}

// ShortFileNameLength returns the current "assumed" padding around short
// file names within the "padded" flags output.  If you don't like the
// default adjust via SetShortFileNameLength()
func ShortFileNameLength() int32 {
	return atomic.LoadInt32(&shortFileNameLength)
}

// SetShortFileNameLength will set the "assumed" padding around short
// file names within the "padded" flags output.  To get the current
// setting see ShortFileNameLength()
func SetShortFileNameLength(length int32) {
	atomic.StoreInt32(&shortFileNameLength, length)
}

// LongFileNameLength returns the current "assumed" padding around long
// file names within the "padded" flags output.  If you don't like the
// default adjust via SetLongFileNameLength()
func LongFileNameLength() int32 {
	return atomic.LoadInt32(&longFileNameLength)
}

// SetLongFileNameLength will set the "assumed" padding around long
// file names within the "padded" flags output.  To get the current
// setting see LongFileNameLength()
func SetLongFileNameLength(length int32) {
	atomic.StoreInt32(&longFileNameLength, length)
}

// ShortFuncNameLength returns the current "assumed" padding around short
// func names within the "padded" flags output.  If you don't like the
// default adjust via SetShortFuncNameLength()
func ShortFuncNameLength() int32 {
	return atomic.LoadInt32(&shortFuncNameLength)
}

// SetShortFuncNameLength will set the "assumed" padding around short
// func names within the "padded" flags output.  To get the current
// setting see ShortFuncNameLength()
func SetShortFuncNameLength(length int32) {
	atomic.StoreInt32(&shortFuncNameLength, length)
}

// LongFuncNameLength returns the current "assumed" padding around long
// func names within the "padded" flags output.  If you don't like the
// default adjust via SetLongFuncNameLength()
func LongFuncNameLength() int32 {
	return atomic.LoadInt32(&longFuncNameLength)
}

// SetLongFuncNameLength will set the "assumed" padding around long
// func names within the "padded" flags output.  To get the current
// setting see LongFuncNameLength()
func SetLongFuncNameLength(length int32) {
	atomic.StoreInt32(&longFuncNameLength, length)
}

// CallDepth is to retrieve the current call depth... see SetCallDepth for
// details if needed.
func CallDepth() int32 {
	return atomic.LoadInt32(&callDepth)
}

// SetCallDepth is for runtime.Caller() to identify where a Noteln() or Print()
// or Issuef() (etc) was called from (so meta-data dumped in "extended"
// mode gives the correct calling function and line number).  The existing
// value is correct *but* if you choose to further wrap 'out' methods in
// some extra method layer (or two) in your own modules then you might
// want to increase it via this public package global.  See callDepth()
// to get the current setting.
func SetCallDepth(depth int32) {
	atomic.StoreInt32(&callDepth, depth)
}

// ErrorExitVal returns the current preferred "failure" exit value for the
// out package (defaults to -1).  Returns an int32 due to use of sync/atomic
// for setting this internally.
func ErrorExitVal() int32 {
	return atomic.LoadInt32(&errorExitVal)
}

// SetErrorExitVal will set the preferred error exit value (what the pkg 'out'
// uses on os.Exit() for fatals and such).  Please pass in an int32 ... and
// it'll be safely updated.
func SetErrorExitVal(val int32) {
	atomic.StoreInt32(&errorExitVal, val)
}

// String implements a stringer for the Level type so we can print out string
// representations for the level setting, these names map to the "code" names
// for these settings (not the prefixes for the setting since some levels have
// no output prefix by default).  Client still has full control over "primary"
// out prefix separately from this, see SetPrefix and such.
func (l Level) String() string {
	lvl2String := map[Level]string{
		LevelTrace:   "TRACE",
		LevelDebug:   "DEBUG",
		LevelVerbose: "VERBOSE",
		LevelInfo:    "INFO",
		LevelNote:    "NOTE",
		LevelIssue:   "ISSUE",
		LevelError:   "ERROR",
		LevelFatal:   "FATAL",
		LevelDiscard: "DISCARD",
	}
	l = levelCheck(l)
	return lvl2String[l]
}

// LevelString2Level takes the string representation of a level and turns
// it back into a Level type (integer type/iota)
func LevelString2Level(s string) Level {
	string2Lvl := map[string]Level{
		"TRACE":   LevelTrace,
		"DEBUG":   LevelDebug,
		"VERBOSE": LevelVerbose,
		"INFO":    LevelInfo,
		"NOTE":    LevelNote,
		"ISSUE":   LevelIssue,
		"ERROR":   LevelError,
		"FATAL":   LevelFatal,
		"DISCARD": LevelDiscard,
	}
	if _, ok := string2Lvl[s]; !ok {
		Fatalln("Invalid string level:", s, ", unable to map to Level type")
	}
	return string2Lvl[s]
}

// Prefix returns the current prefix for the given log level
func Prefix(level Level) string {
	level = levelCheck(level)
	if level == LevelDiscard {
		Fatalln("Prefix is not defined for level discard, should never be requested")
	}
	var prefix string
	for _, o := range outputters {
		o.mu.RLock()
		defer o.mu.RUnlock()
		if o.level == level {
			prefix = o.prefix
			break
		}
	}
	return prefix
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
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.level == level {
			o.prefix = prefix
		}
	}
}

// Discard disables all screen and/or logfile output, can be done via
// SetThreshold() as well (directly) or via SetWriter() to something
// like ioutil.Discard or bufio io.Writer if you want to capture output.
// Anyhow, this is a quick way to disable output (if outputTgt is not set
// to out.ForScreen or out.ForLogfile or both | together nothing happens)
func Discard(outputTgt int) {
	if outputTgt&ForScreen != 0 {
		SetThreshold(LevelDiscard, ForScreen)
	}
	if outputTgt&ForLogfile != 0 {
		SetThreshold(LevelDiscard, ForLogfile)
	}
}

// Flags gets the screen or logfile output flags (Ldate, Ltime, .. above),
// you must give one or the other (out.ForScreen or out.ForLogfile) only.
func Flags(level Level, outputTgt int) int {
	level = levelCheck(level)
	var flags int
	for _, o := range outputters {
		o.mu.RLock()
		sF := o.screenFlags
		lF := o.logFlags
		outLvl := o.level
		o.mu.RUnlock()
		if outLvl == level {
			if outputTgt&ForScreen != 0 {
				flags = sF
			} else if outputTgt&ForLogfile != 0 {
				flags = lF
			} else {
				Fatalln("Invalid identification of screen or logfile target for Flags()")
			}
			break
		}
	}
	return flags
}

// SetFlags sets the screen and/or logfile output flags (Ldate, Ltime, .. above)
// Note: This can set flags for a specific log level or for all log levels if
// one uses out.LevelAll for the 1st arg, the 2nd arg is the flags to set
// and the 3rd is what to set them on (out.ForScreen, out.ForLogfile, or
// out.ForBoth)
func SetFlags(level Level, flags int, outputTgt int) {
	for _, o := range outputters {
		o.mu.Lock()
		defer o.mu.Unlock()
		if level == LevelAll || o.level == level {
			if outputTgt&ForScreen != 0 {
				o.screenFlags = flags
			}
			if outputTgt&ForLogfile != 0 {
				o.logFlags = flags
			}
			if level != LevelAll {
				break
			}
		}
	}
}

// Writer gets the screen or logfile output io.Writer for the given log
// level, outputTgt is out.ForScreen or out.ForLogfile depending upon which
// writer you want to grab for the given logging level
func Writer(level Level, outputTgt int) io.Writer {
	level = levelCheck(level)
	writer := ioutil.Discard
	for _, o := range outputters {
		o.mu.RLock()
		defer o.mu.RUnlock()
		if o.level == level {
			if outputTgt&ForScreen != 0 {
				writer = o.screenHndl
			}
			if outputTgt&ForLogfile != 0 {
				writer = o.logfileHndl
			}
		}
	}
	return writer
}

// SetWriter sets the screen and/or logfile output io.Writer for every log
// level to the given writer
func SetWriter(level Level, w io.Writer, outputTgt int) {
	for _, o := range outputters {
		o.mu.Lock()
		defer o.mu.Unlock()
		if level == LevelAll || o.level == level {
			if outputTgt&ForScreen != 0 {
				o.screenHndl = w
			}
			if outputTgt&ForLogfile != 0 {
				o.logfileHndl = w
			}
			if level != LevelAll {
				break
			}
		}
	}
}

// ResetNewline allows one to reset the screen and/or logfile LvlOutput so the
// next bit of output either "thinks" (or doesn't) that the previous output put
// the user on a new line.  If 'val' is true then the next output run through
// this pkg to the given output stream can be prefixed (with timestamps, etc),
// if it is false then no prefix, eg: out.Note("Enter data: ") might produce:
//   Note: enter data: <prompt>
// Which leaves the output stream thinking the last msg had no newline at the
// end of string.  Now, if one's input method reads input with the user hitting
// a newline then the below call can be used to tell the LvlOutput(s) that a
// newline was hit and any fresh output can be prefixed cleanly:
//   out.ResetNewline(true, out.ForScreen|out.ForLogfile)
// Note: for any *output* running through this module this is auto-handled
func ResetNewline(val bool, outputTgt int) {
	// Safely adjust these settings
	mutex.Lock()
	{
		if outputTgt&ForScreen != 0 {
			screenNewline = val
		}
		if outputTgt&ForLogfile != 0 {
			logfileNewline = val
		}
	}
	mutex.Unlock()
}

// LogFileName returns any known log file name (if none returns "")
func LogFileName() string {
	mutex.Lock()
	safeLogFileName := logFileName
	mutex.Unlock()
	return safeLogFileName
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
	// Safely adjust this global settings
	mutex.Lock()
	{
		logFileName = file.Name()
	}
	mutex.Unlock()
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
	// Safely adjust these settings
	mutex.Lock()
	logFileName = file.Name()
	safeLogFileName := logFileName
	mutex.Unlock()
	for _, o := range outputters {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.logfileHndl = file
	}
	return safeLogFileName
}

// Next we head into the <Level>() class methods which don't add newlines
// and simply space separate the options sent to them:

// Trace is the most verbose debug level, space separate opts with no newline
// added and is by default prefixed with "Trace: <date/time> <msg>" for each
// line but you can use flags and remove the timestamp, can also drop the prefix
func Trace(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	TRACE.output(terminate, exitVal, v...)
}

// Debug is meant for basic debugging, space separate opts with no newline added
// and is, by default, prefixed with "Debug: <date/time> <your msg>" for each
// line but you can use flags and remove the timestamp, can also drop the prefix
func Debug(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	DEBUG.output(terminate, exitVal, v...)
}

// Verbose meant for verbose user seen screen output, space separated
// opts printed with no newline added, no output prefix is added by default
func Verbose(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	VERBOSE.output(terminate, exitVal, v...)
}

// Print is meant for "normal" user output, space separated opted
// printed with no newline added, no output prefix is added by default
func Print(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	INFO.output(terminate, exitVal, v...)
}

// Info is the same as Print: meant for "normal" user output, space separated
// opts printed with no newline added and no output prefix added by default
func Info(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	INFO.output(terminate, exitVal, v...)
}

// Note is meant for output of key "note" the user should pay attention to, opts
// space separated and printed with no newline added, "Note: <msg>" prefix is
// also added by default
func Note(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	NOTE.output(terminate, exitVal, v...)
}

// Issue is meant for "normal" user error output, space separated opts
// printed with no newline added, "Issue: <msg>" prefix added by default,
// if you want to exit after the issue is reported see IssueExit()
func Issue(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	ISSUE.output(terminate, exitVal, v...)
}

// IssueExit is meant for "normal" user error output, space separated opts
// printed with no newline added, "Issue: <msg>" prefix added by default,
// the "exit" form of this output routine results in os.Exit() being
// called with the given exitVal (see Issue() if you do not want to exit)
func IssueExit(exitVal int, v ...interface{}) {
	mutex.Lock()
	terminate := true
	mutex.Unlock()
	ISSUE.output(terminate, exitVal, v...)
}

// Error is meant for "unexpected"/system error output, space separated
// opts printed with no newline added, "Error: <msg>" prefix added by default,
// if you want to exit after erroring see ErrorExit()
// Note: by "unexpected" these are things like filesystem permissions
// problems, see Issue for more normal user level usage issues
func Error(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	ERROR.output(terminate, exitVal, v...)
}

// ErrorExit is meant for "unexpected"/system error output, space separated
// opts printed with no newline added, "Error: <msg>" prefix added by default,
// the "exit" form of this output routine results in os.Exit() being called
// with given exitVal (see Error() if you don't want to exit)
// Note: by "unexpected" these are things like filesystem permissions
// problems, see Issue for more normal user level usage issues
func ErrorExit(exitVal int, v ...interface{}) {
	mutex.Lock()
	terminate := true
	mutex.Unlock()
	ERROR.output(terminate, exitVal, v...)
}

// Fatal is meant for "unexpected"/system fatal error output, space separated
// opts printed with no newline added, "Fatal: <msg>" prefix added by default
// and the tool will exit non-zero here
func Fatal(v ...interface{}) {
	mutex.Lock()
	terminate := true
	exitVal := int(atomic.LoadInt32(&errorExitVal))
	mutex.Unlock()
	FATAL.output(terminate, exitVal, v...)
}

// Next we head into the <Level>ln() class methods which add newlines
// and space separate the options sent to them:

// Traceln is the most verbose debug level, space separate opts with newline
// added and is, by default, prefixed with "Trace: <your output>" for each line
// but you can use flags and remove the timestamp, can also drop the prefix
func Traceln(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	TRACE.outputln(terminate, exitVal, v...)
}

// Debugln is meant for basic debugging, space separate opts with newline added
// and is, by default, prefixed with "Debug: <date/time> <yourmsg>" for each
// line but you can use flags and remove the timestamp, can also drop the prefix
func Debugln(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	DEBUG.outputln(terminate, exitVal, v...)
}

// Verboseln is meant for verbose user seen screen output, space separated
// opts printed with newline added, no output prefix is added by default
func Verboseln(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	VERBOSE.outputln(terminate, exitVal, v...)
}

// Println is the same as Infoln: meant for "normal" user output, space
// separated opts printed with newline added and no output prefix added by
// default
func Println(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	INFO.outputln(terminate, exitVal, v...)
}

// Infoln is the same as Println: meant for "normal" user output, space
// separated opts printed with newline added and no output prefix added by
// default
func Infoln(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	INFO.outputln(terminate, exitVal, v...)
}

// Noteln is meant for output of key items the user should pay attention to,
// opts are space separated and printed with a newline added, "Note: <msg>"
// prefix is also added by default
func Noteln(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	NOTE.outputln(terminate, exitVal, v...)
}

// Issueln is meant for "normal" user error output, space separated
// opts printed with a newline added, "Issue: <msg>" prefix added by default
// Note: by "normal" these are things like unknown codebase name given, etc...
// for unexpected errors use Errorln (eg: file system full, etc).  If you wish
// to exit after your issue is printed please use IssueExitln() instead.
func Issueln(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	ISSUE.outputln(terminate, exitVal, v...)
}

// IssueExitln is meant for "normal" user error output, space separated opts
// printed with a newline added, "Issue: <msg>" prefix added by default,
// the "exit" form of this output routine results in os.Exit() being called
// with the given exitVal.  See Issueln() if you do not want to exit.  This
// routine honors PKG_OUT_STACK_TRACE_CONFIG env as well as the package
// stacktrace setting via SetStackTraceConfig(), see that routine for docs.
func IssueExitln(exitVal int, v ...interface{}) {
	mutex.Lock()
	terminate := true
	mutex.Unlock()
	ISSUE.outputln(terminate, exitVal, v...)
}

// Errorln is meant for "unexpected"/system error output, space separated
// opts printed with a newline added, "Error: <msg>" prefix added by default
// Note: by "unexpected" these are things like filesystem permissions problems,
// see Noteln/Issueln for more normal user level notes/usage
func Errorln(v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	ERROR.outputln(terminate, exitVal, v...)
}

// ErrorExitln is meant for "unexpected"/system error output, space separated
// opts printed with a newline added, "Error: <msg>" prefix added by default,
// the "exit" form of this output routine results in os.Exit() being called
// with given exitVal.  If you don't want to exit use Errorln() instead.  This
// routine honors PKG_OUT_STACK_TRACE_CONFIG env as well as the package
// stacktrace setting via SetStackTraceConfig(), see that routine for docs.
// Note: by "unexpected" these are things like filesystem permissions
// problems, see IssueExitln() for more normal user level usage issues
func ErrorExitln(exitVal int, v ...interface{}) {
	mutex.Lock()
	terminate := true
	mutex.Unlock()
	ERROR.outputln(terminate, exitVal, v...)
}

// Fatalln is meant for "unexpected"/system fatal error output, space separated
// opts printed with a newline added, "Fatal: <msg>" prefix added by default
// and the tool will exit non-zero here.  Note that a stacktrace can be set up
// via the env PKG_OUT_STACK_TRACE_CONFIG or the API SetStackTraceConfig(),
// see the routine for docs.
func Fatalln(v ...interface{}) {
	mutex.Lock()
	terminate := true
	exitVal := int(atomic.LoadInt32(&errorExitVal))
	mutex.Unlock()
	FATAL.outputln(terminate, exitVal, v...)
}

// Next we head into the <Level>f() class methods which take a standard
// format string for go (see 'godoc fmt' and look at Printf() if needed):

// Tracef is the most verbose debug level, format string followed by args and
// output is, by default, prefixed with "Trace: <date/time> <your msg>" for each
// line but you can use flags and remove the timestamp, can also drop the prefix
func Tracef(format string, v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	TRACE.outputf(terminate, exitVal, format, v...)
}

// Debugf is meant for basic debugging, format string followed by args and
// output is by default prefixed with "Debug: <date/time> <your msg>" for each
// line but you can use flags and remove the timestamp, can also drop the prefix
func Debugf(format string, v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	DEBUG.outputf(terminate, exitVal, format, v...)
}

// Verbosef is meant for verbose user seen screen output, format string
// followed by args (and no output prefix is added by default)
func Verbosef(format string, v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	VERBOSE.outputf(terminate, exitVal, format, v...)
}

// Printf is the same as Infoln: meant for "normal" user output, format string
// followed by args (and no output prefix added by default)
func Printf(format string, v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	INFO.outputf(terminate, exitVal, format, v...)
}

// Infof is the same as Printf: meant for "normal" user output, format string
// followed by args (and no output prefix added by default)
func Infof(format string, v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	INFO.outputf(terminate, exitVal, format, v...)
}

// Notef is meant for output of key "note" the user should pay attention to,
// format string followed by args, "Note: <yourmsg>" prefixed by default
func Notef(format string, v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	NOTE.outputf(terminate, exitVal, format, v...)
}

// Issuef is meant for "normal" user error output, format string followed
// by args, prefix "Issue: <msg>" added by default.  If you want to exit
// after your issue see IssueExitf() instead.
func Issuef(format string, v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	ISSUE.outputf(terminate, exitVal, format, v...)
}

// IssueExitf is meant for "normal" user error output, format string followed
// by args, prefix "Issue: <msg>" added by default, the "exit" form of this
// output routine results in os.Exit() being called with the given exitVal.
// If you do not want to exit then see Issuef() instead
func IssueExitf(exitVal int, format string, v ...interface{}) {
	mutex.Lock()
	terminate := true
	mutex.Unlock()
	ISSUE.outputf(terminate, exitVal, format, v...)
}

// Errorf is meant for "unexpected"/system error output, format string
// followed by args, prefix "Error: <msg>" added by default
// Note: by "unexpected" these are things like filesystem permissions problems,
// see Notef/Issuef for more normal user level notes/usage
func Errorf(format string, v ...interface{}) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	ERROR.outputf(terminate, exitVal, format, v...)
}

// ErrorExitf is meant for "unexpected"/system error output, format string
// followed by args, prefix "Error: <msg>" added by default, the "exit" form
// of this output routine results in os.Exit() being called with given exitVal
func ErrorExitf(exitVal int, format string, v ...interface{}) {
	mutex.Lock()
	terminate := true
	mutex.Unlock()
	ERROR.outputf(terminate, exitVal, format, v...)
}

// Fatalf is meant for "unexpected"/system fatal error output, format string
// followed by args, prefix "Fatal: <msg>" added by default and will exit
// non-zero from the tool (see Go 'log' Fatalf() method)
func Fatalf(format string, v ...interface{}) {
	mutex.Lock()
	terminate := true
	exitVal := int(atomic.LoadInt32(&errorExitVal))
	mutex.Unlock()
	FATAL.outputf(terminate, exitVal, format, v...)
}

// Exit is meant for terminating without messaging but supporting stack trace
// dump settings and such (*only* if non-zero exit).
func Exit(exitVal int) {
	if exitVal != 0 {
		FATAL.exit(exitVal)
	} else {
		INFO.exit(exitVal)
	}
}

// SetStackTraceConfig can be used to control when stack traces are dumped
// in errors (or issues/warnings).  The settings are controlled via these
// flags (defined globally for this pkg):
//   ForScreen   // stack traces are only sent to the "screen" stream
//   ForLogfile  // stack traces are only sent to the "logfile" stream (deafult)
//   ForBoth     // stack traces go to both the screen and the logfile
// These flags *must* be combined with the following flags to indicate when
// stack traces are dumped:
//   StackTraceNonZeroErrorExit // use for stacktrace shown on non-zero exit
//   StackTraceErrorExit        // use for stacktrace shown on any exit
//   StackTraceAllIssues        // use for stacktrace for any/all warning/errs
// Combine a flag from each of the above to indicate how you wish stack traces
// to be handled by Issue*/Error*/Fatal* and related mechanisms (0=no stack msg)
// One can also use the env PKG_OUT_STACK_TRACE_CONFIG set to comma separated
// settings, eg: "screen,nonzeroerrorexit" or "both,allissues", if invalid
// it will be ignored and no stack traces will dump based on the env settings.
// Note: One cannot, now, differentiate stack trace settings between screen and
// logfile settings (ie: you currently cannot have stack traces to the logfile
// for say any issue while having stack traces to the screen for non-zero exit
// issues... although one could extend this module if desired for that).
func SetStackTraceConfig(cfg int) {
	// Safely adjust these settings, switch to atomic perhaps
	mutex.Lock()
	{
		stackTraceConfig = cfg
	}
	mutex.Unlock()
}

// getStackTrace will get a stack trace (of the desired depth) and return
// it.  Currently callDepth is used assuming this is being called from the
// defined routes into the 'out' pkg (ie: this will map to where 'out' was
// called or used from basically, ignoring the various methods in this pkg
// so as to give a stack trace relative to the users code).
func getStackTrace(detErr DetailedError, depth ...int) string {
	var myStack string
	if detErr != nil {
		// If we have a DetailedError we can get the innermost stack tarce so
		// we have the most detail possible in our stack trace:
		var errLines []string
		var origStack string
		shallow := false
		fillErrorInfo(detErr, shallow, &errLines, &origStack)
		myStack = "\nStack Trace: " + origStack + "\n"
	} else {
		// Not a DetailedError, lets get a stack trace relative to the call
		// to the 'out' pkg API (eg: out.Error("whatever"), where user called)
		myDepth := int(atomic.LoadInt32(&callDepth))
		if depth != nil {
			myDepth = depth[0]
		}
		trace, _ := stackTrace(myDepth)
		myStack = fmt.Sprintf("\nStack Trace: %s\n", trace)
	}
	return myStack
}

// InsertPrefix takes a multiline string (potentially) and for each
// line places a string prefix in front of each line unless told to
// skip the 1st line perhaps (or to insert blanks of the prefix length).
// The parameters:
// - s: the string to prefix, can be multi-line (\n separated)
// - prefix: the desired string prefix to insert
// - ctrl: how to insert the prefix (can be combined via 'or')
//     AlwaysInsert            // Prefix every line, regardless of output history
//     BlankInsert             // Only spaces inserted (same length as prefix)
//     SkipFirstLine           // 1st line in multi-line string has no prefix
//     SmartInsert             // See doPrefixing(), only handled there now
// - errCode: attempt to insert any valid error code into the prefix, eg:
//     // a prefix of "Error: " would become "Error #<errcode>: "
func InsertPrefix(s string, prefix string, ctrl int, errCode int) string {
	// FEATURE: add ability to prefix the 1st line only (smartly or always) and
	//          then "blank prefix" the rest of the lines (readability better?)

	if prefix == "" {
		return s
	}
	if ctrl&AlwaysInsert != 0 {
		ctrl = 0 // turn off everything, always means *always*
	}
	// If there is an error code of interest then insert it into the message
	// if possible... braindead, must be something like "Error: " or "Issue: "
	// and so a split on ":" results in two strings, results: "Error #<code>: "
	if errCode > 0 && errCode != int(defaultErrCode) {
		parts := strings.Split(prefix, ":")
		if len(parts) == 2 {
			prefix = parts[0] + fmt.Sprintf(" #%d:", errCode) + parts[1]
		}
	}
	pfxLength := len(prefix)
	format := "%" + fmt.Sprintf("%d", pfxLength) + "s"
	spacePrefix := fmt.Sprintf(format, "")
	lines := strings.Split(s, "\n")
	numLines := len(lines)
	newLines := []string{}
	for idx, line := range lines {
		if (idx == numLines-1 && line == "") ||
			(idx == 0 && ctrl&SkipFirstLine != 0) {
			// if last line and it's empty don't prefix it, add empty line or if
			// it's the 1st line and we are to skip prefixing the 1st line:
			newLines = append(newLines, line)
		} else if ctrl&BlankInsert != 0 {
			// if blank-only prefix desired then go with that for all lines
			newLines = append(newLines, spacePrefix+line)
		} else {
			// otherwise prefix every line with given prefix
			newLines = append(newLines, prefix+line)
		}
	}
	newstr := strings.Join(newLines, "\n")
	return newstr
}

// getAnyDetailedErrors will determine if, given a list of interfaces, any of
// them are of interface type DetailedError and, if so, push them onto a
// slice of DetailedError's
func getAnyDetailedErrors(v ...interface{}) []DetailedError {
	var detErrs []DetailedError

	for _, item := range v {
		currErr, ok := item.(DetailedError)
		if ok {
			detErrs = append(detErrs, currErr)
		}
	}
	return detErrs
}

// output is similar to fmt.Print(), it'll space separate args with no newline
// and output them to the screen and/or log file loggers based on levels
func (o *LvlOutput) output(terminal bool, exitVal int, v ...interface{}) {
	detErrs := getAnyDetailedErrors(v...)
	var detErr DetailedError
	if detErrs != nil {
		detErr = detErrs[0]
	}
	if detErr != nil {
		// if we have a detailed error coming in at some output level insure
		// that the output level used for that output matches the incoming
		// output level always
		detErr.SetLvlOut(o)
	}
	// set up the message to dump
	msg := fmt.Sprint(v...)

	// dump msg based on screen and log output levels
	_, err := o.stringOutput(msg, terminal, exitVal, detErr)
	if err != nil {
		mutex.Lock()
		{
			fmt.Fprintf(os.Stderr, "%s", err)
		}
		mutex.Unlock()
		mutex.RLock()
		if deferFunc != nil {
			deferFunc(int(atomic.LoadInt32(&errorExitVal)))
		}
		mutex.RUnlock()
		if os.Getenv("PKG_OUT_NO_EXIT") != "1" {
			os.Exit(int(atomic.LoadInt32(&errorExitVal)))
		}
	}
}

// outputln is similar to fmt.Println(), it'll space separate args with no
// newline and output them to the screen and/or log file loggers based on levels
func (o *LvlOutput) outputln(terminal bool, exitVal int, v ...interface{}) {
	// set up the message to dump
	msg := fmt.Sprintln(v...)

	detErrs := getAnyDetailedErrors(v...)
	var detErr DetailedError
	if detErrs != nil {
		detErr = detErrs[0]
	}

	// dump msg based on screen and log output levels
	_, err := o.stringOutput(msg, terminal, exitVal, detErr)
	if err != nil {
		mutex.Lock()
		{
			fmt.Fprintf(os.Stderr, "%s", err)
		}
		mutex.Unlock()
		mutex.RLock()
		if deferFunc != nil {
			deferFunc(int(atomic.LoadInt32(&errorExitVal)))
		}
		mutex.RUnlock()
		if os.Getenv("PKG_OUT_NO_EXIT") != "1" {
			os.Exit(int(atomic.LoadInt32(&errorExitVal)))
		}
	}
}

// outputf is similar to fmt.Printf(), it takes a format and args and outputs
// the resulting string to the screen and/or log file loggers based on levels
func (o *LvlOutput) outputf(terminal bool, exitVal int, format string, v ...interface{}) {
	// set up the message to dump
	msg := fmt.Sprintf(format, v...)

	detErrs := getAnyDetailedErrors(v...)
	var detErr DetailedError
	if detErrs != nil {
		detErr = detErrs[0]
	}

	// dump msg based on screen and log output levels
	_, err := o.stringOutput(msg, terminal, exitVal, detErr)
	if err != nil {
		mutex.Lock()
		{
			fmt.Fprintf(os.Stderr, "%s", err)
		}
		mutex.Unlock()
		mutex.RLock()
		if deferFunc != nil {
			deferFunc(int(atomic.LoadInt32(&errorExitVal)))
		}
		mutex.RUnlock()
		if os.Getenv("PKG_OUT_NO_EXIT") != "1" {
			os.Exit(int(atomic.LoadInt32(&errorExitVal)))
		}
	}
}

// stackTraceWanted will decide if the client wants a stack trace in their
// output stream to the screen or to the logfile based on if the tool is
// dying ("terminal" here means exitting the program after dumping errs),
// and if we have a non-zero exit value or not... and how stack traces have
// been set up by the client (via API or env settings, env takes precendence)
func (o *LvlOutput) stackTraceWanted(terminal bool, exitVal int, outputTgt int) bool {
	mutex.Lock()
	stackCfg := stackTraceConfig
	defer mutex.Unlock()
	val := os.Getenv("PKG_OUT_STACK_TRACE_CONFIG")
	if val != "" {
		newCfg := 0
		settings := strings.Split(val, ",")
		if len(settings) == 2 {
			for _, currSetting := range settings {
				currSetting = strings.ToLower(currSetting)
				switch currSetting {
				case "both":
					newCfg = newCfg | ForBoth
				case "screen":
					newCfg = newCfg | ForScreen
				case "logfile":
					newCfg = newCfg | ForLogfile
				case "nonzeroerrorexit":
					newCfg = newCfg | StackTraceNonZeroErrorExit
				case "errorexit":
					newCfg = newCfg | StackTraceErrorExit
				case "allissues", "all":
					newCfg = newCfg | StackTraceAllIssues
				case "off":
					newCfg = 0
				default:
				}
			}
			stackCfg = newCfg
		}
	}
	// See if our output target (screen|logfile) wants a stack trace or not...
	if stackCfg&outputTgt == 0 {
		return false
	}
	o.mu.RLock()
	level := o.level
	o.mu.RUnlock()
	// Now see if the detailed config really implies a stack trace is wanted...
	if stackCfg&StackTraceNonZeroErrorExit != 0 {
		// config indicates only terminal non-zero exit should have stack trace
		if !terminal || exitVal == 0 {
			// this isn't a terminal non-zero exit so no stack trace
			return false
		}
	} else if stackCfg&StackTraceErrorExit != 0 {
		// config indicates any warning/error level issue needs a stack trace
		if !terminal || level < LevelIssue {
			// error isn't a warning/error level and/or it's not fatal, no trace
			return false
		}
	} else if stackCfg&StackTraceAllIssues != 0 {
		// config indicates just any warning (issue) or err needs a stack trace
		if level < LevelIssue {
			// no trace if level is Note, Print/Info, Verbose, Debug, Trace
			return false
		}
	} else {
		// config currently doesn't indicate stack trace of any kind wanted
		return false
	}
	// if we made it here then a stack trace is desired by the client and
	// all pre-requisites for the given type of stack trace config are met:
	return true
}

// exit will use os.Exit() to bail with the given exitVal, if
// that exitVal is non-zero and a stracktrace is set up it will
// dump that stacktrace as well (honoring all log levels and such),
// see getStackTrace() for the env and package settings honored.
func (o *LvlOutput) exit(exitVal int) {
	// get the stacktrace if it's configured, note that the depth is
	// a little shallower if coming straight through Exit() to here:
	mutex.Lock()
	stacktrace := getStackTrace(nil, int(CallDepth())-1)
	terminal := true
	safeLogThreshold := logThreshold
	safeScreenThreshold := screenThreshold
	mutex.Unlock()
	o.mu.RLock()
	level := o.level
	o.mu.RUnlock()
	if stacktrace != "" && o.stackTraceWanted(terminal, exitVal, ForScreen) && level >= safeScreenThreshold && level != LevelDiscard {
		msg, _, suppressOutput := o.doPrefixing(stacktrace, ForScreen, SmartInsert, nil, false)
		if !suppressOutput && msg != "" {
			mutex.Lock()
			_, err := o.screenHndl.Write([]byte(msg))
			if err != nil {
				fmt.Fprintf(os.Stderr, "%sError writing stacktrace to screen output handle:\n%+v\n", o.prefix, err)
				mutex.Unlock()
				mutex.RLock()
				if deferFunc != nil {
					deferFunc(int(atomic.LoadInt32(&errorExitVal)))
				}
				mutex.RUnlock()
				if os.Getenv("PKG_OUT_NO_EXIT") != "1" {
					os.Exit(int(atomic.LoadInt32(&errorExitVal)))
				}
				mutex.Lock()
			}
			mutex.Unlock()
		}
	}
	if stacktrace != "" && o.stackTraceWanted(terminal, exitVal, ForLogfile) && level >= safeLogThreshold && level != LevelDiscard {
		msg, _, suppressOutput := o.doPrefixing(stacktrace, ForLogfile, SmartInsert, nil, false)
		if !suppressOutput && msg != "" {
			o.logfileHndl.Write([]byte(msg))
		}
	}
	mutex.RLock()
	if deferFunc != nil {
		deferFunc(exitVal)
	}
	mutex.RUnlock()
	if os.Getenv("PKG_OUT_NO_EXIT") != "1" {
		os.Exit(exitVal)
	}
}

// itoa converts an int to fixed-width decimal ASCII.  Give a negative width to
// avoid zero-padding.  Knows the buffer has capacity.  Taken from Go's 'log'
// pkg since we want some of the same formatting.
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
// to construct a string to put in the log file (uses the flags settings
// to decide what metadata to print, ie: one can "or" together different
// flags to identify what should be dumped, like the Go 'log' package but
// more flags are available, see top of file)
func getFlagString(buf *[]byte, flags int, level Level, funcName string, file string, line int, t time.Time) string {
	if flags&Lpid != 0 {
		pid := os.Getpid()
		*buf = append(*buf, '[')
		itoa(buf, pid, 1)
		*buf = append(*buf, "] "...)
	}
	if flags&Llevel != 0 {
		lvl := fmt.Sprintf("%-8s", level)
		*buf = append(*buf, lvl...)
	}
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
		formatLen := int(atomic.LoadInt32(&longFileNameLength))
		if flags&Lshortfile != 0 {
			formatLen = int(atomic.LoadInt32(&shortFileNameLength))
			short := file
			for i := len(file) - 1; i > 0; i-- {
				if file[i] == '/' {
					short = file[i+1:]
					break
				}
			}
			file = short
		}
		var tmpbuf []byte
		tmpslice := &tmpbuf
		*tmpslice = append(*tmpslice, file...)
		*tmpslice = append(*tmpslice, ':')
		itoa(tmpslice, line, -1)
		if flags&Lshortfunc != 0 {
			formatLen = formatLen + int(atomic.LoadInt32(&shortFuncNameLength))
			parts := strings.Split(funcName, ".")
			var justFunc string
			if len(parts) > 1 {
				justFunc = parts[len(parts)-1]
			} else {
				justFunc = "???"
			}
			*tmpslice = append(*tmpslice, ':')
			*tmpslice = append(*tmpslice, justFunc...)
		} else if flags&Llongfunc != 0 {
			formatLen = formatLen + int(atomic.LoadInt32(&longFuncNameLength))
			*tmpslice = append(*tmpslice, ':')
			*tmpslice = append(*tmpslice, funcName...)
		} else {
			*tmpslice = append(*tmpslice, ' ')
		}

		// Note that this length stuff is weak, if you have long filenames,
		// long func names or long paths to func's it won't do much good as
		// it's currently written (or if you have different flags across
		// different log levels... but if consistent then it can help a bit)
		formatStr := "%-" + fmt.Sprintf("%d", formatLen) + "s: "
		str := fmt.Sprintf(formatStr, string(*tmpslice))
		*buf = append(*buf, str...)
	}
	return fmt.Sprintf("%s", *buf)
}

// determineFlags takes a set of flags defined in an env var (string) that
// can be comma separated and turns them into a real flags store type (int) with
// the desired settings, allows easy dynamic tweaking or addition of flags in
// screen output for instance
func determineFlags(flagStr string) int {
	flagStrs := strings.Split(flagStr, ",")
	flags := 0
	for _, currFlag := range flagStrs {
		switch currFlag {
		case "debug":
			flags |= Llevel | Ltime | Lmicroseconds | Lshortfile | Lshortfunc
		case "all":
			flags |= Lpid | Llevel | Ldate | Ltime | Lmicroseconds | Lshortfile | Lshortfunc
		case "longall":
			flags |= Lpid | Llevel | Ldate | Ltime | Lmicroseconds | Llongfile | Llongfunc
		case "pid":
			flags |= Lpid
		case "level":
			flags |= Llevel
		case "date":
			flags |= Ldate
		case "time":
			flags |= Ltime
		case "micro", "microseconds":
			flags |= Lmicroseconds
		case "file", "shortfile":
			flags |= Lshortfile
		case "longfile":
			flags |= Llongfile
		case "func", "shortfunc":
			flags |= Lshortfunc
		case "longfunc":
			flags |= Llongfunc
		case "off":
			flags = 0
			break
		default:
		}
	}
	return flags
}

// insertFlagMetadata basically checks to see what flags are set for
// the current screen or logfile output and inserts the meta-data in
// front of the string, see InsertPrefix for ctrl description, outputTgt
// here is either ForScreen or ForLogfile (constants) for output.  Note that
// it will also return a boolean to indicate if the output should be supressed
// or not (typically not but one can filter debug/trace output and if one has
// set PKG_OUT_DEBUG_SCOPE, see env var elsewhere in this pkg for doc), params:
//	s (string): the string to insert flag meta-data into
//	outputTgt (int): where output goes, ForScreen, ForLogfile or ForBoth
//	ctrl (int): how to insert the prefix (can be combined via 'or')
//		AlwaysInsert      // Prefix every line, regardless of output history
//		BlankInsert       // Only spaces inserted (same length as prefix)
//		SkipFirstLine     // 1st line in multi-line string has no prefix
//		SmartInsert       // See doPrefixing(), only handled there now
//	overrideFlags (*int): get flags not from 'o' but here, else set to nil
//	ignoreEnv (bool): ignore any env overrides/filters (eg: formatter wants all)
// Returns the update msg string, any flag metadata available and if the output
// should be suppressed (such as if debug scope doesn't include this module)
func (o *LvlOutput) insertFlagMetadata(s string, outputTgt int, ctrl int, overrideFlags *int, ignoreEnv bool, depth ...int) (string, *FlagMetadata, bool) {
	now := time.Now() // do this before Caller below, can take some time
	var file, funcName string
	var line, flags int
	var suppressOutput bool
	var level Level
	flagMetadata := &FlagMetadata{}
	var callerDepth int
	if depth != nil {
		callerDepth = depth[0]
	} else {
		callerDepth = int(atomic.LoadInt32(&callDepth))
	}
	o.mu.RLock()
	lvlOutLevel := o.level
	sF := o.screenFlags
	lF := o.logFlags
	if overrideFlags != nil {
		sF = *overrideFlags
		lF = *overrideFlags
	}
	o.mu.RUnlock()
	flagMetadata.Level = fmt.Sprintf("%s", lvlOutLevel)
	flagMetadata.Time = &now
	// if printing to the screen target use those flags, else use logfile flags
	if outputTgt&ForScreen != 0 {
		if str := os.Getenv("PKG_OUT_SCREEN_FLAGS"); !ignoreEnv && str != "" {
			flags = determineFlags(str)
		} else {
			flags = sF
		}
		level = lvlOutLevel
	} else if outputTgt&ForLogfile != 0 {
		if str := os.Getenv("PKG_OUT_LOGFILE_FLAGS"); !ignoreEnv && str != "" {
			flags = determineFlags(str)
		} else {
			flags = lF
		}
		level = lvlOutLevel
	} else {
		Fatalln("Invalid target passed to insertFlagMetadata():", outputTgt)
	}
	suppressOutput = false
	if flags&(Lshortfile|Llongfile|Lshortfunc|Llongfunc) != 0 ||
		(!ignoreEnv && os.Getenv("PKG_OUT_DEBUG_SCOPE") != "") {
		var ok bool
		var pc uintptr
		pc, file, line, ok = runtime.Caller(callerDepth)
		if !ok {
			file = "???"
			line = 0
			funcName = "???"
		} else {
			f := runtime.FuncForPC(pc)
			if f == nil {
				funcName = "???"
			} else {
				funcName = f.Name()
			}
		}
		if !ignoreEnv {
			// If the user has restricted debugging output to specific packages
			// or methods (funcname might be "github.com/dvln/out.MethodName")
			// then suppress all debug output outside of the desired scope and
			// only show those packages or methods of interest... simple substr
			// match is done currently
			if debugScope := os.Getenv("PKG_OUT_DEBUG_SCOPE"); funcName != "???" && debugScope != "" && (lvlOutLevel == LevelDebug || lvlOutLevel == LevelTrace) {
				scopeParts := strings.Split(debugScope, ",")
				suppressOutput = true
				for _, scopePart := range scopeParts {
					if strings.Contains(funcName, scopePart) {
						suppressOutput = false
						break
					}
				}
			}
		}
		flagMetadata.Func = funcName
		flagMetadata.File = filepath.Base(file)
		flagMetadata.Path = filepath.Dir(file)
		flagMetadata.LineNo = line
	}
	o.mu.Lock()
	o.buf = o.buf[:0]
	leader := getFlagString(&o.buf, flags, level, funcName, file, line, now)
	flagMetadata.PID = os.Getpid()
	o.mu.Unlock()
	if leader == "" {
		return s, flagMetadata, suppressOutput
	}
	// Use 0 as the error code as we don't want to try and insert any error
	// code in standard flags prefix (that's only needed for errs/warnings),
	// so just do a full prefixing of the flags data
	s = InsertPrefix(s, leader, ctrl, 0)
	return s, flagMetadata, suppressOutput
}

// doPrefixing takes the users output string and decides how to prefix
// the users message based on the log level and any associated prefix,
// eg: "Debug: ", as well as any flag settings that could add date/time
// and information on the calling Go file and line# and such.  Params:
// - s: the string/message to prefix (can be multi-line)
// - outputTgt: where output is going, ForScreen or ForLogfile
// - ctrl: how to insert the prefix (can be combined via 'or')
//     AlwaysInsert            // Prefix every line, regardless of output history
//     BlankInsert             // Only spaces inserted (same length as prefix)
//     SkipFirstLine           // 1st line in multi-line string has no prefix
//     SmartInsert             // Attempts to track newlines for output targets
//                             // (Sceen|Log) and only prefixes the 1st line if
//                             // it is on a fresh new line (ie: will "or" in
//                             // SkipFirstLine to AlwaysInsert if not on fresh)
// - detErr: a detailed error *if* one is available, else nil
// - checkSuppressOnly: basically says skip all prefixing but still do the
// calculation to see if we should dump this line based on trace/debug scope
// info (which can only be calculated once we figure out what pkg/func is
// being dumped... which, you guessed it, happens right here now).
// Routine returns:
// - s (string): the prefixed string (no pfx added if checkSuppressOnly is true)
// - suppressOutput (bool): indicates if output should be suppressed due to
//               some log level restriction, eg: see PKG_OUT_DEBUG_SCOPE
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
// The only thing we "lose" here potentially is that the line that prints
// the username isn't prefixed to keep the output clean (no line #15 details)
// hence we don't have a date/timestamp for that "part" of the output and that
// could cause someone to think it was line 14 that was slow if the next entry
// was 20 minutes later (eg: the myfile.go line 16 print statement).  There is
// a mode to turn off smart flags prefixing so one can see such "invisible"
// or missing timestamps on the same line... to do that one would set the env
// PKG_OUT_SMART_FLAGS_PREFIX to "off".  For screen output default settings
// this changes nothing (flags are off for regular/note/issue/err output).
// However, the log file entry differs as we can see in the 3rd line, we
// now see the timestamp and file info for both parts of that line:
//   <date/time> myfile.go:13: Note: This is a test
//   <date/time> myfile.go:13: Note: and only a test
//   <date/time> myfile.go:14: Note: that I am showing to <date/time> myfile:15: John
// Obviously makes the output uglier but might be of use at some point.
//
// One more note, if a stack trace is added (based on current stack trace cfg)
// then routine will forcibly add a newline if the fatal doesn't have one and
// and dump stack trace after that, eg (both means screen and logfile output):
//   os.Setenv("PKG_OUT_STACK_TRACE_CONFIG", "both,nonzeroerrorexit")
//   out.Fatal("Severe error, giving up\n")    [use better errors of course]
// Screen output:
//   Fatal: Severe error, giving up
//   Fatal:
//   Fatal: Stack Trace: <multiline stacktrace here>
// Log file entry:
//   <date/time> myfile.go:37: Fatal: Severe error, giving up
//   <date/time> myfile.go:37: Fatal:
//   <date/time> myfile.go:37: Fatal: Stack Trace: <multiline stacktrace here>
func (o *LvlOutput) doPrefixing(s string, outputTgt int, ctrl int, detErr DetailedError, checkSuppressOnly bool) (string, *FlagMetadata, bool) {
	// Where we check out if we previously had no newline and if so the
	// first line (if multiline) will not have the prefix, see example
	// in function header around username
	origString := s
	var onNewline bool
	mutex.Lock()
	scrNewline := screenNewline
	logNewline := logfileNewline
	mutex.Unlock()
	if outputTgt&ForScreen != 0 {
		onNewline = scrNewline
	} else if outputTgt&ForLogfile != 0 {
		onNewline = logNewline
	} else {
		Fatalln("Invalid target for output given in doPrefixing():", outputTgt)
	}
	if !onNewline && ctrl&SmartInsert != 0 {
		ctrl = ctrl | SkipFirstLine
	}
	errCode := int(defaultErrCode)
	if detErr != nil {
		errCode = Code(detErr)
	}
	o.mu.RLock()
	prefix := o.prefix
	o.mu.RUnlock()
	// Insert prefix for this logging level
	s = InsertPrefix(s, prefix, ctrl, errCode)

	if os.Getenv("PKG_OUT_SMART_FLAGS_PREFIX") == "off" {
		ctrl = AlwaysInsert // forcibly add prefix without smarts
	}
	// Now set up metadata prefix (eg: timestamp), if any, same as above
	// it has the brains to not add in a prefix if not needed or wanted
	var suppressOutput bool
	var flagMetadata *FlagMetadata
	s, flagMetadata, suppressOutput = o.insertFlagMetadata(s, outputTgt, ctrl, nil, false)
	if checkSuppressOnly {
		s = origString // use non-pfx string *but* return suppressOutput result
	}
	return s, flagMetadata, suppressOutput
}

// writeOutput basically sends the output to the io.Writer for the given
// output stream.  It can add in stack traces if they have been requested
// (ie: output level indicates Issue, Error or Fatal and set up for it).
// This can write to screen or logfile depending upon params:
// - s (string): the string to write
// - outputTgt (int): either ForScreen or ForLogfile (nothing else valid)
// - dying (bool): indicates we are about to die (can add newlines then)
// - exitVal (int): what exit value is (only used if dying is true)
// - stacktrace (string): if given and stack requested it will be added, note
// that it is already pre-formatted
// Returns:
// - int: number of bytes written to the io.Writer associated with outputTgt
// - error: if any unexpected write error occurred this will be a raw Go error
func (o *LvlOutput) writeOutput(s string, outputTgt int, dying bool, exitVal int, stacktrace string) (int, error) {
	tgtString := "logfile"
	o.mu.RLock()
	prefix := o.prefix
	hndl := o.logfileHndl
	o.mu.RUnlock()
	mutex.Lock()
	tgtStreamNewline := &logfileNewline
	mutex.Unlock()
	if outputTgt&ForScreen == 1 {
		tgtString = "screen"
		o.mu.RLock()
		hndl = o.screenHndl
		o.mu.RUnlock()
		mutex.Lock()
		tgtStreamNewline = &screenNewline
		mutex.Unlock()
	}
	writeLength := 0

	// Safely do writes and adjust settings as needed
	mutex.Lock()
	n, err := hndl.Write([]byte(s))
	mutex.Unlock()
	writeLength += n
	if err != nil {
		mutex.Lock()
		writeErr := fmt.Errorf("%sError writing to %s output handler:\n%+v\noutput:\n%s\n", prefix, tgtString, err, s)
		mutex.Unlock()
		return writeLength, writeErr
	}
	mutex.Lock()
	if s[len(s)-1] == 0x0A { // if last char is a newline..
		*tgtStreamNewline = true
	} else {
		*tgtStreamNewline = false
	}
	if dying && !*tgtStreamNewline {
		// ignore errors, just quick "prettyup" attempt:
		n, err = hndl.Write([]byte("\n"))
		writeLength += n
		if err != nil {
			writeErr := fmt.Errorf("%sError writing newline to %s output handler:\n%+v\n", prefix, tgtString, err)
			mutex.Unlock()
			return writeLength, writeErr
		}
		// normally we're dying so this doesn't matter but in testing we can
		// suppress the dying/exit so lets put 'out' into the right state
		*tgtStreamNewline = true
	}
	mutex.Unlock()
	// See if stack trace is needed...
	if o.stackTraceWanted(dying, exitVal, outputTgt) {
		mutex.Lock()
		n, err = hndl.Write([]byte(stacktrace))
		mutex.Unlock()
		writeLength += n
		if err != nil {
			mutex.Lock()
			writeErr := fmt.Errorf("%sError writing stacktrace to %s output handle:\n%+v\n", prefix, tgtString, err)
			mutex.Unlock()
			return writeLength, writeErr
		}
	}
	return writeLength, nil
}

// stringOutput uses existing screen and log levels to decide what, if
// anything, is printed to the screen and/or log file Writer(s) based on
// current screen and log output thresholds, flags and stack trace settings.
// It returns the length of output written (to *both* screen and logfile targets
// if it succeeds... and note that the length will include additional meta-data
// that the user has requested be added) and an error if one occurred (only
// one error will be considered if you pass in multiples, just the 1st).
// WARNING: this will silently ignore multiple detailed errors if you give it
// more than one and simply use the 1st one given (that syntax is just used
// to make the parameter optional to the stringOutput() method)
func (o *LvlOutput) stringOutput(s string, dying bool, exitVal int, detErrs ...DetailedError) (int, error) {
	// print to the screen output writer first...
	var detErr DetailedError
	if detErrs != nil {
		detErr = detErrs[0]
	}
	var err error
	var screenLength int
	var logfileLength int

	// Try and insure goroutine safety as we read and write *LvlOutput
	o.mu.RLock()
	level := o.level
	formatter := o.formatter
	o.mu.RUnlock()

	mutex.Lock()
	forScreen := ForScreen
	forLogfile := ForLogfile
	smartInsert := SmartInsert
	safeScreenThreshold := screenThreshold
	safeLogThreshold := logThreshold
	mutex.Unlock()

	// Grab the best stack trace we can find to use in case it's needed, but
	// only for Issue, Error and Fatal levels of output (currently)... pass
	// through any detailed error given by the user
	var stackStr, screenStackTrace, logfileStackTrace string
	if level >= LevelIssue {
		stackStr = getStackTrace(detErr)
		screenStackTrace = stackStr
		logfileStackTrace = stackStr
		if !o.stackTraceWanted(dying, exitVal, forScreen) {
			screenStackTrace = ""
		}
		if !o.stackTraceWanted(dying, exitVal, forLogfile) {
			logfileStackTrace = ""
		}
	}
	// Allow any plugin formatter to independently format only one type of
	// output if desired (screen only or log only), or both.  From here on we
	// start independently tracking the screen and logfile output details
	screenStr := s
	logfileStr := s
	screenNoOutputMask := 0
	logfileNoOutputMask := 0
	screenSkipNativePfx := false
	logfileSkipNativePfx := false
	if formatter != nil {
		// If the client has registered a formatting interface method then
		// lets give it a spin, may adjust the output or suppress it alltogether
		// to the screen and/or logfile.  Right now it gets the generic stack
		// trace but usually one wouldn't want to use it since the built-in
		// formatting (below) will add the stack trace and timestamps and such
		// to the clients returned message (unless told not to)... but if that
		// is suppressed perhaps the clients wants to do something with it in
		// their newly formatted message... perhaps not.
		code := int(DefaultErrCode())
		if detErr != nil {
			code = Code(detErr)
		}
		applyMask := 0
		noOutputMask := 0
		skipNativePfx := false
		var resultStr string
		// Cheat a little and grab detailed output flags metadata for formatter,
		// note that it will include the pid, level and date info automatically
		flags := Llongfile | Llongfunc
		_, flagMetadata, _ := o.insertFlagMetadata(s, forScreen, AlwaysInsert, &flags, true, 4)
		if stackStr != "" {
			flagMetadata.Stack = stackStr
		}
		resultStr, applyMask, noOutputMask, skipNativePfx = formatter.FormatMessage(s, level, code, dying, *flagMetadata)
		// Based on formatter results set up screen and logfile output & controls
		if applyMask&forScreen != 0 {
			screenNoOutputMask = noOutputMask
			screenSkipNativePfx = skipNativePfx
			screenStr = resultStr
		}
		if applyMask&forLogfile != 0 {
			logfileNoOutputMask = noOutputMask
			logfileSkipNativePfx = skipNativePfx
			logfileStr = resultStr
		}
	}

	// Lets see if screen (here) or logfile (below) output is active:
	if level >= safeScreenThreshold && level != LevelDiscard && screenNoOutputMask&forScreen == 0 {
		// Screen output active based on output levels (and formatters, if any)
		pfxScreenStr, _, suppressOutput := o.doPrefixing(screenStr, forScreen, smartInsert, detErr, screenSkipNativePfx)

		// Note that suppressOutput is for suppressing trace/debug output so
		// only selected/desired packages have debug output dumped (currently)
		if !suppressOutput {
			pfxStackTrace := ""
			if screenStackTrace != "" {
				pfxStackTrace, _, _ = o.doPrefixing(screenStackTrace, forScreen, smartInsert, detErr, screenSkipNativePfx)
			}
			screenLength, err = o.writeOutput(pfxScreenStr, forScreen, dying, exitVal, pfxStackTrace)
			if err != nil {
				return screenLength, err
			}
		}
	}

	// Print to the log file writer next (if needed):
	if level >= safeLogThreshold && level != LevelDiscard && logfileNoOutputMask&forLogfile == 0 {
		pfxLogfileStr, _, suppressOutput := o.doPrefixing(logfileStr, forLogfile, smartInsert, detErr, logfileSkipNativePfx)

		// Note that suppressOutput is for suppressing trace/debug output so
		// only selected/desired packages have debug output dumped (currently)
		if !suppressOutput {
			pfxStackTrace := ""
			if logfileStackTrace != "" {
				pfxStackTrace, _, _ = o.doPrefixing(logfileStackTrace, forLogfile, smartInsert, detErr, logfileSkipNativePfx)
			}
			logfileLength, err = o.writeOutput(pfxLogfileStr, forLogfile, dying, exitVal, pfxStackTrace)
			if err != nil {
				return logfileLength + screenLength, err
			}
		}
	}
	// if we're dying off then we need to exit unless overrides in play,
	// this env var should be used for test suites only really...
	if dying {
		mutex.RLock()
		if deferFunc != nil {
			deferFunc(int(atomic.LoadInt32(&errorExitVal)))
		}
		mutex.RUnlock()
		if os.Getenv("PKG_OUT_NO_EXIT") != "1" {
			os.Exit(int(atomic.LoadInt32(&errorExitVal)))
		}
	}
	// if all good return all the bytes we wrote to *both* targets and nil err
	return logfileLength + screenLength, nil
}

// LevelWriter will return an io.Writer compatible structure for the desired
// output level.  It's a bit cheesy but does the trick if you want an
// io.Writer at a given level.  Typically one would not use this and
// would instead just pass in out.TRACE, out.DEBUG, out.VERBOSE, out.INFO,
// out.NOTE, out.ISSUE, out.ERROR or out.FATAL directly as the io.Writer
// to write at a given output level (but if you have a Level type and
// want to get the associated io.Writer you can use this method)
func LevelWriter(l Level) *LvlOutput {
	var writeLevel *LvlOutput
	l = levelCheck(l)
	switch l {
	case LevelTrace:
		writeLevel = TRACE
	case LevelDebug:
		writeLevel = DEBUG
	case LevelVerbose:
		writeLevel = VERBOSE
	case LevelInfo:
		writeLevel = INFO
	case LevelNote:
		writeLevel = NOTE
	case LevelIssue:
		writeLevel = ISSUE
	case LevelError:
		writeLevel = ERROR
	case LevelFatal:
		writeLevel = FATAL
	default:
		writeLevel = INFO
	}
	return writeLevel
}

// Write implements an io.Writer interface for any of the available output
// levels.  Use LevelWriter() above to grab a *LvlOutput structure for the
// desired output level... so, if you want the "standard" info (print) output
// level then one might do this to get an io.Writer for that level:
//   infoWriter := out.LevelWriter(out.LevelInfo)
//   fmt.Fprintf(infoWriter, "%s\n", stringVar)
// Note: one could also use the INFO writer directly and more easily:
//   fmt.Fprintf(out.INFO, "%s\n", stringVar)
// The above example would print to the screen and any logfile that was set up
// just like the Info[ln|f]() (ie: Print[ln|f]()) routine would.  Please keep in
// mind that if a logfile has been activated this io.Writer will behave somewhat
// like an io.MultiWriter (writing to multiple target handles potentially, the
// difference being that here the different target handles can be augmented with
// independently controlled levels of additional meta-data, independent output
// levels for each target handle, etc (and one could combine this io.Writer with
// additional writers itself via io.MultiWriter even, crazy fun)
func (o *LvlOutput) Write(p []byte) (n int, err error) {
	mutex.Lock()
	terminate := false
	exitVal := 0
	mutex.Unlock()
	return o.stringOutput(string(p), terminate, exitVal)
}

// stackTrace returns a copy of the error with the stack trace field populated
// and any other shared initialization; skips 'skip' levels of the stack trace.
// The cleaned up "current" stack trace is returned as is anything that might
// be visible after it as 'context'.  This was borrowed from Dropbox's open
// 'errors' package and frankly I'm not clear as to if 'context' is ever
// non-empty (based on stack traces I've seen and the parsing below I think
// it will always be empty but I might be missing something)
// NOTE: This can panic if any error (eg: runtime stack trace gathering issue)
func stackTrace(skip int) (string, string) {
	// grow buf until it's large enough to store entire stack trace
	buf := make([]byte, 128)
	for {
		n := runtime.Stack(buf, false)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, len(buf)*2)
	}

	// Returns the index of the first occurrence of '\n' in the buffer 'b'
	// starting with index 'start'.
	//
	// In case no occurrence of '\n' is found, it returns len(b). This
	// simplifies the logic on the calling sites.
	indexNewline := func(b []byte, start int) int {
		if start >= len(b) {
			return len(b)
		}
		searchBuf := b[start:]
		index := bytes.IndexByte(searchBuf, '\n')
		if index == -1 {
			return len(b)
		}
		return (start + index)
	}

	// Strip initial levels of stack trace, but keep header line that
	// identifies the current goroutine.
	var strippedBuf bytes.Buffer
	index := indexNewline(buf, 0)
	if index != -1 {
		strippedBuf.Write(buf[:index])
	}

	// Skip lines.
	for i := 0; i < skip; i++ {
		index = indexNewline(buf, index+1)
		index = indexNewline(buf, index+1)
	}

	isDone := false
	startIndex := index
	lastIndex := index
	for !isDone {
		index = indexNewline(buf, index+1)
		if (index - lastIndex) <= 1 {
			isDone = true
		} else {
			lastIndex = index
		}
	}
	strippedBuf.Write(buf[startIndex:index])
	return strippedBuf.String(), string(buf[index:])
}
