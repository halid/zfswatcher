//
// zparse.go
//
// Copyright © 2012-2013 Damicon Kraa Oy
//
// This file is part of zfswatcher.
//
// Zfswatcher is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Zfswatcher is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with zfswatcher. If not, see <http://www.gnu.org/licenses/>.
//

package main

import (
	"errors"
	"runtime"
	"strings"
	"zfswatcher.damicon.fi/notifier"
)

// ZFS pool disk usage.
type PoolUsageType struct {
	Name          string
	Avail         int64
	Used          int64
	Usedsnap      int64
	Usedds        int64
	Usedrefreserv int64
	Usedchild     int64
	Refer         int64
	Mountpoint    string
}

// Parse "zfs list -H -o name,avail,used,usedsnap,usedds,usedrefreserv,usedchild,refer,mountpoint" command output.
func parseZfsList(str string) map[string]*PoolUsageType {
	usagemap := make(map[string]*PoolUsageType)
	for lineno, line := range strings.Split(str, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 9 {
			notify.Printf(notifier.CRIT, "invalid line %d in ZFS usage output: %s",
				lineno+1, line)
			notify.Attach(notifier.CRIT, str)
			continue
		}
		usagemap[f[0]] = &PoolUsageType{
			Name:          f[0],
			Avail:         unniceNumber(f[1]),
			Used:          unniceNumber(f[2]),
			Usedsnap:      unniceNumber(f[3]),
			Usedds:        unniceNumber(f[4]),
			Usedrefreserv: unniceNumber(f[5]),
			Usedchild:     unniceNumber(f[6]),
			Refer:         unniceNumber(f[7]),
			Mountpoint:    f[8],
		}
	}
	return usagemap
}

// This represents ZFS disk/container/whatever.
type DevEntry struct {
	name      string
	state     string
	read      int64
	write     int64
	cksum     int64
	rest      string
	subDevs   []int
	parentDev int
}

// Parse ZFS zpool status config section and return a tree of the volumes/containers/devices.
func parseConfstr(confstr string) (devs []*DevEntry, err error) {
	if confstr == "The configuration cannot be determined." {
		return nil, errors.New("configuration can not be determined")
	}
	var prevIndent int
	var devStack []int

	for _, line := range strings.Split(confstr, "\n") {
		if line == "" {
			continue
		}
		origlen := len(line)
		line = strings.TrimLeft(line, " ")
		indent := (origlen - len(line)) / 2
		f := strings.Fields(line)
		if f[0] == "NAME" && f[1] == "STATE" && f[2] == "READ" && f[3] == "WRITE" && f[4] == "CKSUM" {
			continue
		}
		// make a new dev entry
		var dev DevEntry
		dev.name = f[0]
		// set up defaults
		dev.read = -1
		dev.write = -1
		dev.cksum = -1
		if len(f) > 1 {
			dev.state = f[1]
		}
		if len(f) > 2 {
			dev.read = unniceNumber(f[2])
		}
		if len(f) > 3 {
			dev.write = unniceNumber(f[3])
		}
		if len(f) > 4 {
			dev.cksum = unniceNumber(f[4])
		}
		if len(f) > 5 {
			dev.rest = strings.Join(f[5:], " ")
		}

		switch {
		case indent == 0: // root level entry
			dev.parentDev = -1
			devs = append(devs, &dev)
			thisDev := len(devs) - 1
			devStack = []int{thisDev} // reset + push
		case indent > prevIndent: // subdev of the previous entry
			dev.parentDev = devStack[len(devStack)-1]
			devs = append(devs, &dev)
			thisDev := len(devs) - 1
			devs[dev.parentDev].subDevs = append(devs[dev.parentDev].subDevs, thisDev)
			devStack = append(devStack, thisDev) // push
		case indent == prevIndent: // same level as the previous entry
			devStack = devStack[:len(devStack)-1] // pop
			dev.parentDev = devStack[len(devStack)-1]
			devs = append(devs, &dev)
			thisDev := len(devs) - 1
			devs[dev.parentDev].subDevs = append(devs[dev.parentDev].subDevs, thisDev)
			devStack = append(devStack, thisDev) // push
		case indent < prevIndent: // dedent
			devStack = devStack[:len(devStack)-1-(prevIndent-indent)] // pop x N
			dev.parentDev = devStack[len(devStack)-1]
			devs = append(devs, &dev)
			thisDev := len(devs) - 1
			devs[dev.parentDev].subDevs = append(devs[dev.parentDev].subDevs, thisDev)
			devStack = append(devStack, thisDev) // push
		}
		prevIndent = indent
	}
	return devs, nil
}

// A single ZFS pool.
type PoolType struct {
	name    string
	state   string
	status  string
	action  string
	see     string
	scan    string
	devs    []*DevEntry
	errors  string
	infostr string
}

// Internal parser state for parseZpoolStatus() function.
type zpoolStatusParserState int

const (
	stSTART zpoolStatusParserState = iota
	stPOOL
	stSTATE
	stSTATUS
	stACTION
	stSEE
	stSCAN
	stCONFIG
	stERRORS
)

// Parse "zpool status" output.
func parseZpoolStatus(zpoolStatusOutput string) (pools []*PoolType, err error) {
	// catch a panic which might occur during parsing if we get something unexpected:
	defer func() {
		if p := recover(); p != nil {
			// get the panic location:
			buf := make([]byte, 4096)
			length := runtime.Stack(buf, false)
			str := string(buf[:length]) // truncate trailing garbage
			notify.Printf(notifier.CRIT, "panic parsing status output: %v", p)
			notify.Attach(notifier.CRIT, str+"\n"+zpoolStatusOutput)
			// force the return value err to be true:
			err = errors.New("panic parsing status output")
		}
	}()

	var curpool *PoolType
	var confstr string
	var poolinfostr string

	var s zpoolStatusParserState = stSTART

	for lineno, line := range strings.Split(zpoolStatusOutput, "\n") {
		poolinfostr += line + "\n"

		// this state machine implements a parser:
		switch {
		case s == stSTART && line == "no pools available":
			// pools will be empty slice
			return pools, nil
		case s == stSTART && len(line) >= 8 && line[:8] == "  pool: ":
			curpool = &PoolType{name: line[8:]}
			s = stPOOL
		case s == stPOOL && len(line) >= 8 && line[:8] == " state: ":
			curpool.state = line[8:]
			s = stSTATE
		case s == stSTATE && len(line) >= 8 && line[:8] == "status: ":
			curpool.status = line[8:]
			s = stSTATUS
		case s == stSTATUS && len(line) >= 1 && line[:1] == "\t":
			curpool.status += "\n" + line[1:]
		case s == stSTATUS && len(line) >= 8 && line[:8] == "action: ":
			curpool.action = line[8:]
			s = stACTION
		case s == stACTION && len(line) >= 1 && line[:1] == "\t":
			curpool.action += "\n" + line[1:]
		case (s == stSTATE || s == stACTION) && len(line) >= 8 &&
			line[:8] == "   see: ":
			curpool.see = line[8:]
			s = stSEE
		case (s == stSTATE || s == stACTION || s == stSEE) &&
			len(line) >= 7 && line[:7] == " scan: ":
			curpool.scan = line[7:]
			s = stSCAN
		// fix for 240245896aad46d0d41b0f9f257ff2abd09cb29b
		// released in zfs-0.6.0-rc14
		case (s == stSTATE || s == stACTION || s == stSEE) &&
			len(line) >= 8 && line[:8] == "  scan: ":
			curpool.scan = line[8:]
			s = stSCAN
		case s == stSCAN && len(line) >= 1 && line[:1] == "\t":
			curpool.scan += "\n" + line[1:]
		case s == stSCAN && len(line) >= 4 && line[:4] == "    ":
			curpool.scan += "\n" + line[4:]
		case (s == stSCAN || s == stSTATE || s == stACTION || s == stSEE) &&
			len(line) >= 7 && line[:7] == "config:":
			s = stCONFIG
			if line[7:] != "" {
				confstr = line[7:]
			}
		case s == stCONFIG && line == "":
			// skip
		case s == stCONFIG && len(line) >= 1 && line[:1] == "\t":
			confstr += "\n" + line[1:]
		case s == stCONFIG && len(line) >= 8 && line[:8] == "errors: ":
			curpool.errors = line[8:]
			s = stERRORS
		case s == stERRORS && line == "":
			// this is the end of a pool!
			curpool.devs, err = parseConfstr(confstr)
			if err != nil {
				notify.Print(notifier.ERR, "device configuration parse error: %s", err)
				notify.Attach(notifier.ERR, confstr)
			}
			confstr = ""
			curpool.infostr = poolinfostr
			poolinfostr = ""
			pools = append(pools, curpool)
			s = stSTART
		default:
			notify.Printf(notifier.CRIT, "invalid line %d in status output: %s", lineno+1, line)
			notify.Attach(notifier.CRIT, zpoolStatusOutput)
			return pools, errors.New("parser error")
		}
	}
	return pools, nil
}

// eof