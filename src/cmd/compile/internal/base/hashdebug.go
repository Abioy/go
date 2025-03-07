// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package base

import (
	"bytes"
	"cmd/internal/notsha256"
	"cmd/internal/obj"
	"cmd/internal/src"
	"fmt"
	"internal/bisect"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type writeSyncer interface {
	io.Writer
	Sync() error
}

type hashAndMask struct {
	// a hash h matches if (h^hash)&mask == 0
	hash uint64
	mask uint64
	name string // base name, or base name + "0", "1", etc.
}

type HashDebug struct {
	mu   sync.Mutex // for logfile, posTmp, bytesTmp
	name string     // base name of the flag/variable.
	// what file (if any) receives the yes/no logging?
	// default is os.Stdout
	logfile          writeSyncer
	posTmp           []src.Pos
	bytesTmp         bytes.Buffer
	matches          []hashAndMask // A hash matches if one of these matches.
	excludes         []hashAndMask // explicitly excluded hash suffixes
	bisect           *bisect.Matcher
	fileSuffixOnly   bool // for Pos hashes, remove the directory prefix.
	inlineSuffixOnly bool // for Pos hashes, remove all but the most inline position.
}

// SetFileSuffixOnly controls whether hashing and reporting use the entire
// file path name, just the basename.  This makes hashing more consistent,
// at the expense of being able to certainly locate the file.
func (d *HashDebug) SetFileSuffixOnly(b bool) *HashDebug {
	d.fileSuffixOnly = b
	return d
}

// SetInlineSuffixOnly controls whether hashing and reporting use the entire
// inline position, or just the most-inline suffix.  Compiler debugging tends
// to want the whole inlining, debugging user problems (loopvarhash, e.g.)
// typically does not need to see the entire inline tree, there is just one
// copy of the source code.
func (d *HashDebug) SetInlineSuffixOnly(b bool) *HashDebug {
	d.inlineSuffixOnly = b
	return d
}

// The default compiler-debugging HashDebug, for "-d=gossahash=..."
var hashDebug *HashDebug

var FmaHash *HashDebug     // for debugging fused-multiply-add floating point changes
var LoopVarHash *HashDebug // for debugging shared/private loop variable changes

// DebugHashMatch reports whether debug variable Gossahash
//
//  1. is empty (returns true; this is a special more-quickly implemented case of 4 below)
//
//  2. is "y" or "Y" (returns true)
//
//  3. is "n" or "N" (returns false)
//
//  4. does not explicitly exclude the sha1 hash of pkgAndName (see step 6)
//
//  5. is a suffix of the sha1 hash of pkgAndName (returns true)
//
//  6. OR
//     if the (non-empty) value is in the regular language
//     "(-[01]+/)+?([01]+(/[01]+)+?"
//     (exclude..)(....include...)
//     test the [01]+ exclude substrings, if any suffix-match, return false (4 above)
//     test the [01]+ include substrings, if any suffix-match, return true
//     The include substrings AFTER the first slash are numbered 0,1, etc and
//     are named fmt.Sprintf("%s%d", varname, number)
//     As an extra-special case for multiple failure search,
//     an excludes-only string ending in a slash (terminated, not separated)
//     implicitly specifies the include string "0/1", that is, match everything.
//     (Exclude strings are used for automated search for multiple failures.)
//     Clause 6 is not really intended for human use and only
//     matters for failures that require multiple triggers.
//
// Otherwise it returns false.
//
// Unless Flags.Gossahash is empty, when DebugHashMatch returns true the message
//
//	"%s triggered %s\n", varname, pkgAndName
//
// is printed on the file named in environment variable GSHS_LOGFILE,
// or standard out if that is empty.  "Varname" is either the name of
// the variable or the name of the substring, depending on which matched.
//
// Typical use:
//
//  1. you make a change to the compiler, say, adding a new phase
//
//  2. it is broken in some mystifying way, for example, make.bash builds a broken
//     compiler that almost works, but crashes compiling a test in run.bash.
//
//  3. add this guard to the code, which by default leaves it broken, but does not
//     run the broken new code if Flags.Gossahash is non-empty and non-matching:
//
//     if !base.DebugHashMatch(ir.PkgFuncName(fn)) {
//     return nil // early exit, do nothing
//     }
//
//  4. rebuild w/o the bad code,
//     GOCOMPILEDEBUG=gossahash=n ./all.bash
//     to verify that you put the guard in the right place with the right sense of the test.
//
//  5. use github.com/dr2chase/gossahash to search for the error:
//
//     go install github.com/dr2chase/gossahash@latest
//
//     gossahash -- <the thing that fails>
//
//     for example: GOMAXPROCS=1 gossahash -- ./all.bash
//
//  6. gossahash should return a single function whose miscompilation
//     causes the problem, and you can focus on that.
func DebugHashMatch(pkgAndName string) bool {
	return hashDebug.DebugHashMatch(pkgAndName)
}

func DebugHashMatchPos(pos src.XPos) bool {
	return hashDebug.DebugHashMatchPos(pos)
}

// HasDebugHash returns true if Flags.Gossahash is non-empty, which
// results in hashDebug being not-nil.  I.e., if !HasDebugHash(),
// there is no need to create the string for hashing and testing.
func HasDebugHash() bool {
	return hashDebug != nil
}

func toHashAndMask(s, varname string) hashAndMask {
	l := len(s)
	if l > 64 {
		s = s[l-64:]
		l = 64
	}
	m := ^(^uint64(0) << l)
	h, err := strconv.ParseUint(s, 2, 64)
	if err != nil {
		Fatalf("Could not parse %s (=%s) as a binary number", varname, s)
	}

	return hashAndMask{name: varname, hash: h, mask: m}
}

// NewHashDebug returns a new hash-debug tester for the
// environment variable ev.  If ev is not set, it returns
// nil, allowing a lightweight check for normal-case behavior.
func NewHashDebug(ev, s string, file writeSyncer) *HashDebug {
	if s == "" {
		return nil
	}

	hd := &HashDebug{name: ev, logfile: file}
	if !strings.Contains(s, "/") {
		m, err := bisect.New(s)
		if err != nil {
			Fatalf("%s: %v", ev, err)
		}
		hd.bisect = m
		return hd
	}
	ss := strings.Split(s, "/")
	// first remove any leading exclusions; these are preceded with "-"
	i := 0
	for len(ss) > 0 {
		s := ss[0]
		if len(s) == 0 || len(s) > 0 && s[0] != '-' {
			break
		}
		ss = ss[1:]
		hd.excludes = append(hd.excludes, toHashAndMask(s[1:], fmt.Sprintf("%s%d", "HASH_EXCLUDE", i)))
		i++
	}
	// hash searches may use additional EVs with 0, 1, 2, ... suffixes.
	i = 0
	for _, s := range ss {
		if s == "" {
			if i != 0 || len(ss) > 1 && ss[1] != "" || len(ss) > 2 {
				Fatalf("Empty hash match string for %s should be first (and only) one", ev)
			}
			// Special case of should match everything.
			hd.matches = append(hd.matches, toHashAndMask("0", fmt.Sprintf("%s0", ev)))
			hd.matches = append(hd.matches, toHashAndMask("1", fmt.Sprintf("%s1", ev)))
			break
		}
		if i == 0 {
			hd.matches = append(hd.matches, toHashAndMask(s, fmt.Sprintf("%s", ev)))
		} else {
			hd.matches = append(hd.matches, toHashAndMask(s, fmt.Sprintf("%s%d", ev, i-1)))
		}
		i++
	}
	return hd

}

func hashOf(pkgAndName string, param uint64) uint64 {
	return hashOfBytes([]byte(pkgAndName), param)
}

func hashOfBytes(sbytes []byte, param uint64) uint64 {
	hbytes := notsha256.Sum256(sbytes)
	hash := uint64(hbytes[7])<<56 + uint64(hbytes[6])<<48 +
		uint64(hbytes[5])<<40 + uint64(hbytes[4])<<32 +
		uint64(hbytes[3])<<24 + uint64(hbytes[2])<<16 +
		uint64(hbytes[1])<<8 + uint64(hbytes[0])

	if param != 0 {
		// Because param is probably a line number, probably near zero,
		// hash it up a little bit, but even so only the lower-order bits
		// likely matter because search focuses on those.
		p0 := param + uint64(hbytes[9]) + uint64(hbytes[10])<<8 +
			uint64(hbytes[11])<<16 + uint64(hbytes[12])<<24

		p1 := param + uint64(hbytes[13]) + uint64(hbytes[14])<<8 +
			uint64(hbytes[15])<<16 + uint64(hbytes[16])<<24

		param += p0 * p1
		param ^= param>>17 ^ param<<47
	}

	return hash ^ param
}

// DebugHashMatch returns true if either the variable used to create d is
// unset, or if its value is y, or if it is a suffix of the base-two
// representation of the hash of pkgAndName.  If the variable is not nil,
// then a true result is accompanied by stylized output to d.logfile, which
// is used for automated bug search.
func (d *HashDebug) DebugHashMatch(pkgAndName string) bool {
	return d.DebugHashMatchParam(pkgAndName, 0)
}

func (d *HashDebug) excluded(hash uint64) bool {
	for _, m := range d.excludes {
		if (m.hash^hash)&m.mask == 0 {
			return true
		}
	}
	return false
}

func hashString(hash uint64) string {
	hstr := ""
	if hash == 0 {
		hstr = "0"
	} else {
		for ; hash != 0; hash = hash >> 1 {
			hstr = string('0'+byte(hash&1)) + hstr
		}
	}
	return hstr
}

func (d *HashDebug) match(hash uint64) *hashAndMask {
	for i, m := range d.matches {
		if (m.hash^hash)&m.mask == 0 {
			return &d.matches[i]
		}
	}
	return nil
}

// DebugHashMatchParam returns true if either the variable used to create d is
// unset, or if its value is y, or if it is a suffix of the base-two
// representation of the hash of pkgAndName and param. If the variable is not
// nil, then a true result is accompanied by stylized output to d.logfile,
// which is used for automated bug search.
func (d *HashDebug) DebugHashMatchParam(pkgAndName string, param uint64) bool {
	if d == nil {
		return true
	}

	hash := hashOf(pkgAndName, param)
	if d.bisect != nil {
		if d.bisect.ShouldReport(hash) {
			d.logDebugHashMatch(d.name, pkgAndName, hash, param)
		}
		return d.bisect.ShouldEnable(hash)
	}
	if m := d.match(hash); m != nil {
		d.logDebugHashMatch(m.name, pkgAndName, hash, param)
		return true
	}
	return false
}

// DebugHashMatchPos is similar to DebugHashMatchParam, but for hash computation
// it uses the source position including all inlining information instead of
// package name and path. The mutex locking is more frequent and more granular.
// Note that the default answer for no environment variable (d == nil)
// is "yes", do the thing.
func (d *HashDebug) DebugHashMatchPos(pos src.XPos) bool {
	if d == nil {
		return true
	}
	// Written this way to make inlining likely.
	return d.debugHashMatchPos(Ctxt, pos)
}

func (d *HashDebug) debugHashMatchPos(ctxt *obj.Link, pos src.XPos) bool {
	// TODO: When we remove the old d.match code, we can use
	// d.bisect.Hash instead of the locked buffer, and we can
	// use d.bisect.Visible to decide whether to format a string.
	d.mu.Lock()
	defer d.mu.Unlock()

	b := d.bytesForPos(ctxt, pos)
	hash := hashOfBytes(b, 0)
	if d.bisect != nil {
		if d.bisect.ShouldReport(hash) {
			d.logDebugHashMatchLocked(d.name, string(b), hash, 0)
		}
		return d.bisect.ShouldEnable(hash)
	}

	// Return false for explicitly excluded hashes
	if d.excluded(hash) {
		return false
	}
	if m := d.match(hash); m != nil {
		d.logDebugHashMatchLocked(m.name, string(b), hash, 0)
		return true
	}
	return false
}

// bytesForPos renders a position, including inlining, into d.bytesTmp
// and returns the byte array.  d.mu must be locked.
func (d *HashDebug) bytesForPos(ctxt *obj.Link, pos src.XPos) []byte {
	d.posTmp = ctxt.AllPos(pos, d.posTmp)
	// Reverse posTmp to put outermost first.
	b := &d.bytesTmp
	b.Reset()
	start := len(d.posTmp) - 1
	if d.inlineSuffixOnly {
		start = 0
	}
	for i := start; i >= 0; i-- {
		p := &d.posTmp[i]
		f := p.Filename()
		if d.fileSuffixOnly {
			f = filepath.Base(f)
		}
		fmt.Fprintf(b, "%s:%d:%d", f, p.Line(), p.Col())
		if i != 0 {
			b.WriteByte(';')
		}
	}
	return b.Bytes()
}

func (d *HashDebug) logDebugHashMatch(varname, name string, hash, param uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logDebugHashMatchLocked(varname, name, hash, param)
}

func (d *HashDebug) logDebugHashMatchLocked(varname, name string, hash, param uint64) {
	file := d.logfile
	if file == nil {
		if tmpfile := os.Getenv("GSHS_LOGFILE"); tmpfile != "" {
			var err error
			file, err = os.OpenFile(tmpfile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
			if err != nil {
				Fatalf("could not open hash-testing logfile %s", tmpfile)
				return
			}
		}
		if file == nil {
			file = os.Stdout
		}
		d.logfile = file
	}
	hstr := hashString(hash)
	if len(hstr) > 24 {
		hstr = hstr[len(hstr)-24:]
	}
	// External tools depend on this string
	if param == 0 {
		fmt.Fprintf(file, "%s triggered %s %s\n", varname, name, hstr)
	} else {
		fmt.Fprintf(file, "%s triggered %s:%d %s\n", varname, name, param, hstr)
	}
	// Print new bisect version too.
	if param == 0 {
		fmt.Fprintf(file, "%s %s\n", name, bisect.Marker(hash))
	} else {
		fmt.Fprintf(file, "%s:%d %s\n", name, param, bisect.Marker(hash))
	}
	file.Sync()
}
