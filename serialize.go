// Copyright 2015 Google Inc. All rights reserved
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kati

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"time"
)

const (
	valueTypeRecursive = 'R'
	valueTypeSimple    = 'S'
	valueTypeTSV       = 'T'
	valueTypeUndefined = 'U'
	valueTypeAssign    = 'a'
	valueTypeExpr      = 'e'
	valueTypeFunc      = 'f'
	valueTypeLiteral   = 'l'
	valueTypeNop       = 'n'
	valueTypeParamref  = 'p'
	valueTypeVarref    = 'r'
	valueTypeVarsubst  = 's'
	valueTypeTmpval    = 't'
)

func dumpInt(w io.Writer, i int) {
	v := int32(i)
	err := binary.Write(w, binary.LittleEndian, &v)
	if err != nil {
		panic(err)
	}
}

func dumpString(w io.Writer, s string) {
	dumpInt(w, len(s))
	_, err := io.WriteString(w, s)
	if err != nil {
		panic(err)
	}
}

func dumpBytes(w io.Writer, b []byte) {
	dumpInt(w, len(b))
	_, err := w.Write(b)
	if err != nil {
		panic(err)
	}
}

func dumpByte(w io.Writer, b byte) {
	err := writeByte(w, b)
	if err != nil {
		panic(err)
	}
}

type serializableVar struct {
	Type     string
	V        string
	Origin   string
	Children []serializableVar
}

type serializableDepNode struct {
	Output             int
	Cmds               []string
	Deps               []int
	Parents            []int
	HasRule            bool
	IsOrderOnly        bool
	IsPhony            bool
	ActualInputs       []int
	TargetSpecificVars []int
	Filename           string
	Lineno             int
}

type serializableTargetSpecificVar struct {
	Name  string
	Value serializableVar
}

type serializableGraph struct {
	Nodes   []*serializableDepNode
	Vars    map[string]serializableVar
	Tsvs    []serializableTargetSpecificVar
	Targets []string
	Roots   []string
	ReadMks []*ReadMakefile
	Exports map[string]bool
}

func encGob(v interface{}) string {
	var buf bytes.Buffer
	e := gob.NewEncoder(&buf)
	err := e.Encode(v)
	if err != nil {
		panic(err)
	}
	return buf.String()
}

func encVar(k string, v Var) string {
	var buf bytes.Buffer
	dumpString(&buf, k)
	v.dump(&buf)
	return buf.String()
}

type depNodesSerializer struct {
	nodes     []*serializableDepNode
	tsvs      []serializableTargetSpecificVar
	tsvMap    map[string]int
	targets   []string
	targetMap map[string]int
	done      map[string]bool
}

func newDepNodesSerializer() *depNodesSerializer {
	return &depNodesSerializer{
		tsvMap:    make(map[string]int),
		targetMap: make(map[string]int),
		done:      make(map[string]bool),
	}
}

func (ns *depNodesSerializer) serializeTarget(t string) int {
	id, present := ns.targetMap[t]
	if present {
		return id
	}
	id = len(ns.targets)
	ns.targetMap[t] = id
	ns.targets = append(ns.targets, t)
	return id
}

func (ns *depNodesSerializer) serializeDepNodes(nodes []*DepNode) {
	for _, n := range nodes {
		if ns.done[n.Output] {
			continue
		}
		ns.done[n.Output] = true

		var deps []int
		for _, d := range n.Deps {
			deps = append(deps, ns.serializeTarget(d.Output))
		}
		var parents []int
		for _, d := range n.Parents {
			parents = append(parents, ns.serializeTarget(d.Output))
		}
		var actualInputs []int
		for _, i := range n.ActualInputs {
			actualInputs = append(actualInputs, ns.serializeTarget(i))
		}

		// Sort keys for consistent serialization.
		var tsvKeys []string
		for k := range n.TargetSpecificVars {
			tsvKeys = append(tsvKeys, k)
		}
		sort.Strings(tsvKeys)

		var vars []int
		for _, k := range tsvKeys {
			v := n.TargetSpecificVars[k]
			sv := serializableTargetSpecificVar{Name: k, Value: v.serialize()}
			//gob := encGob(sv)
			gob := encVar(k, v)
			id, present := ns.tsvMap[gob]
			if !present {
				id = len(ns.tsvs)
				ns.tsvMap[gob] = id
				ns.tsvs = append(ns.tsvs, sv)
			}
			vars = append(vars, id)
		}

		ns.nodes = append(ns.nodes, &serializableDepNode{
			Output:             ns.serializeTarget(n.Output),
			Cmds:               n.Cmds,
			Deps:               deps,
			Parents:            parents,
			HasRule:            n.HasRule,
			IsOrderOnly:        n.IsOrderOnly,
			IsPhony:            n.IsPhony,
			ActualInputs:       actualInputs,
			TargetSpecificVars: vars,
			Filename:           n.Filename,
			Lineno:             n.Lineno,
		})
		ns.serializeDepNodes(n.Deps)
	}
}

func makeSerializableVars(vars Vars) (r map[string]serializableVar) {
	r = make(map[string]serializableVar)
	for k, v := range vars {
		r[k] = v.serialize()
	}
	return r
}

func makeSerializableGraph(g *DepGraph, roots []string) serializableGraph {
	ns := newDepNodesSerializer()
	ns.serializeDepNodes(g.nodes)
	v := makeSerializableVars(g.vars)
	return serializableGraph{
		Nodes:   ns.nodes,
		Vars:    v,
		Tsvs:    ns.tsvs,
		Targets: ns.targets,
		Roots:   roots,
		ReadMks: g.readMks,
		Exports: g.exports,
	}
}

func DumpDepGraphAsJSON(g *DepGraph, filename string, roots []string) {
	sg := makeSerializableGraph(g, roots)
	o, err := json.MarshalIndent(sg, " ", " ")
	if err != nil {
		panic(err)
	}
	f, err2 := os.Create(filename)
	if err2 != nil {
		panic(err2)
	}
	f.Write(o)
	err = f.Close()
	if err != nil {
		panic(err)
	}
}

func DumpDepGraph(g *DepGraph, filename string, roots []string) {
	f, err := os.Create(filename)
	if err != nil {
		panic(err)
	}
	e := gob.NewEncoder(f)
	startTime := time.Now()
	sg := makeSerializableGraph(g, roots)
	LogStats("serialize prepare time: %q", time.Since(startTime))
	startTime = time.Now()
	e.Encode(sg)
	LogStats("serialize output time: %q", time.Since(startTime))
	err = f.Close()
	if err != nil {
		panic(err)
	}
}

func cacheFilename(mk string, roots []string) string {
	filename := ".kati_cache." + mk
	for _, r := range roots {
		filename += "." + r
	}
	return url.QueryEscape(filename)
}

func DumpDepGraphCache(g *DepGraph, roots []string) {
	if len(g.readMks) == 0 {
		panic("No Makefile is read")
	}
	cacheFile := cacheFilename(g.readMks[0].Filename, roots)
	for _, mk := range g.readMks {
		// Inconsistent, do not dump this result.
		if mk.State == FileInconsistent {
			if exists(cacheFile) {
				os.Remove(cacheFile)
			}
			return
		}
	}
	DumpDepGraph(g, cacheFile, roots)
}

func deserializeSingleChild(sv serializableVar) Value {
	if len(sv.Children) != 1 {
		panic(fmt.Sprintf("unexpected number of children: %q", sv))
	}
	return deserializeVar(sv.Children[0])
}

func deserializeVar(sv serializableVar) (r Value) {
	switch sv.Type {
	case "literal":
		return literal(sv.V)
	case "tmpval":
		return tmpval([]byte(sv.V))
	case "expr":
		var e Expr
		for _, v := range sv.Children {
			e = append(e, deserializeVar(v))
		}
		return e
	case "varref":
		return &varref{varname: deserializeSingleChild(sv)}
	case "paramref":
		v, err := strconv.Atoi(sv.V)
		if err != nil {
			panic(err)
		}
		return paramref(v)
	case "varsubst":
		return varsubst{
			varname: deserializeVar(sv.Children[0]),
			pat:     deserializeVar(sv.Children[1]),
			subst:   deserializeVar(sv.Children[2]),
		}

	case "func":
		name := deserializeVar(sv.Children[0]).(literal)
		f := funcMap[string(name[1:])]()
		f.AddArg(name)
		for _, a := range sv.Children[1:] {
			f.AddArg(deserializeVar(a))
		}
		return f
	case "funcEvalAssign":
		return &funcEvalAssign{
			lhs: sv.Children[0].V,
			op:  sv.Children[1].V,
			rhs: deserializeVar(sv.Children[2]),
		}
	case "funcNop":
		return &funcNop{expr: sv.V}

	case "simple":
		return &simpleVar{
			value:  sv.V,
			origin: sv.Origin,
		}
	case "recursive":
		return &recursiveVar{
			expr:   deserializeSingleChild(sv),
			origin: sv.Origin,
		}

	case ":=", "=", "+=", "?=":
		return &targetSpecificVar{
			v:  deserializeSingleChild(sv).(Var),
			op: sv.Type,
		}

	default:
		panic(fmt.Sprintf("unknown serialized variable type: %q", sv))
	}
}

func deserializeVars(vars map[string]serializableVar) Vars {
	r := make(Vars)
	for k, v := range vars {
		r[k] = deserializeVar(v).(Var)
	}
	return r
}

func deserializeNodes(g serializableGraph) (r []*DepNode) {
	nodes := g.Nodes
	tsvs := g.Tsvs
	targets := g.Targets
	// Deserialize all TSVs first so that multiple rules can share memory.
	var tsvValues []Var
	for _, sv := range tsvs {
		tsvValues = append(tsvValues, deserializeVar(sv.Value).(Var))
	}

	nodeMap := make(map[string]*DepNode)
	for _, n := range nodes {
		var actualInputs []string
		for _, i := range n.ActualInputs {
			actualInputs = append(actualInputs, targets[i])
		}

		d := &DepNode{
			Output:             targets[n.Output],
			Cmds:               n.Cmds,
			HasRule:            n.HasRule,
			IsOrderOnly:        n.IsOrderOnly,
			IsPhony:            n.IsPhony,
			ActualInputs:       actualInputs,
			Filename:           n.Filename,
			Lineno:             n.Lineno,
			TargetSpecificVars: make(Vars),
		}

		for _, id := range n.TargetSpecificVars {
			sv := tsvs[id]
			d.TargetSpecificVars[sv.Name] = tsvValues[id]
		}

		nodeMap[targets[n.Output]] = d
		r = append(r, d)
	}

	for _, n := range nodes {
		d := nodeMap[targets[n.Output]]
		for _, o := range n.Deps {
			c, present := nodeMap[targets[o]]
			if !present {
				panic(fmt.Sprintf("unknown target: %d (%s)", o, targets[o]))
			}
			d.Deps = append(d.Deps, c)
		}
		for _, o := range n.Parents {
			c, present := nodeMap[targets[o]]
			if !present {
				panic(fmt.Sprintf("unknown target: %d (%s)", o, targets[o]))
			}
			d.Parents = append(d.Parents, c)
		}
	}

	return r
}

func human(n int) string {
	if n >= 10*1000*1000*1000 {
		return fmt.Sprintf("%.2fGB", float32(n)/1000/1000/1000)
	}
	if n >= 10*1000*1000 {
		return fmt.Sprintf("%.2fMB", float32(n)/1000/1000)
	}
	if n >= 10*1000 {
		return fmt.Sprintf("%.2fkB", float32(n)/1000)
	}
	return fmt.Sprintf("%dB", n)
}

func showSerializedNodesStats(nodes []*serializableDepNode) {
	outputSize := 0
	cmdSize := 0
	depsSize := 0
	actualInputSize := 0
	tsvSize := 0
	filenameSize := 0
	linenoSize := 0
	for _, n := range nodes {
		outputSize += 4
		for _, c := range n.Cmds {
			cmdSize += len(c)
		}
		for _ = range n.Deps {
			depsSize += 4
		}
		for _ = range n.ActualInputs {
			actualInputSize += 4
		}
		for _ = range n.TargetSpecificVars {
			tsvSize += 4
		}
		filenameSize += len(n.Filename)
		linenoSize += 4
	}
	size := outputSize + cmdSize + depsSize + actualInputSize + tsvSize + filenameSize + linenoSize
	LogStats("%d nodes %s", len(nodes), human(size))
	LogStats(" output %s", human(outputSize))
	LogStats(" command %s", human(cmdSize))
	LogStats(" deps %s", human(depsSize))
	LogStats(" inputs %s", human(actualInputSize))
	LogStats(" tsv %s", human(tsvSize))
	LogStats(" filename %s", human(filenameSize))
	LogStats(" lineno %s", human(linenoSize))
}

func (v serializableVar) size() int {
	size := 0
	size += len(v.Type)
	size += len(v.V)
	size += len(v.Origin)
	for _, c := range v.Children {
		size += c.size()
	}
	return size
}

func showSerializedVarsStats(vars map[string]serializableVar) {
	nameSize := 0
	valueSize := 0
	for k, v := range vars {
		nameSize += len(k)
		valueSize += v.size()
	}
	size := nameSize + valueSize
	LogStats("%d vars %s", len(vars), human(size))
	LogStats(" name %s", human(nameSize))
	LogStats(" value %s", human(valueSize))
}

func showSerializedTsvsStats(vars []serializableTargetSpecificVar) {
	nameSize := 0
	valueSize := 0
	for _, v := range vars {
		nameSize += len(v.Name)
		valueSize += v.Value.size()
	}
	size := nameSize + valueSize
	LogStats("%d tsvs %s", len(vars), human(size))
	LogStats(" name %s", human(nameSize))
	LogStats(" value %s", human(valueSize))
}

func showSerializedTargetsStats(targets []string) {
	size := 0
	for _, t := range targets {
		size += len(t)
	}
	LogStats("%d targets %s", len(targets), human(size))
}

func showSerializedReadMksStats(readMks []*ReadMakefile) {
	size := 0
	for _, rm := range readMks {
		size += len(rm.Filename) + len(rm.Hash) + 4
	}
	LogStats("%d makefiles %s", len(readMks), human(size))
}

func showSerializedGraphStats(g serializableGraph) {
	showSerializedNodesStats(g.Nodes)
	showSerializedVarsStats(g.Vars)
	showSerializedTsvsStats(g.Tsvs)
	showSerializedTargetsStats(g.Targets)
	showSerializedReadMksStats(g.ReadMks)
}

func deserializeGraph(g serializableGraph) *DepGraph {
	if LogFlag || StatsFlag {
		showSerializedGraphStats(g)
	}
	nodes := deserializeNodes(g)
	vars := deserializeVars(g.Vars)
	return &DepGraph{
		nodes:   nodes,
		vars:    vars,
		readMks: g.ReadMks,
		exports: g.Exports,
	}
}

func LoadDepGraphFromJSON(filename string) *DepGraph {
	f, err := os.Open(filename)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	d := json.NewDecoder(f)
	g := serializableGraph{Vars: make(map[string]serializableVar)}
	err = d.Decode(&g)
	if err != nil {
		panic(err)
	}
	return deserializeGraph(g)
}

func LoadDepGraph(filename string) *DepGraph {
	f, err := os.Open(filename)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	d := gob.NewDecoder(f)
	g := serializableGraph{Vars: make(map[string]serializableVar)}
	err = d.Decode(&g)
	if err != nil {
		panic(err)
	}
	return deserializeGraph(g)
}

func LoadDepGraphCache(makefile string, roots []string) *DepGraph {
	startTime := time.Now()
	defer func() {
		LogStats("Cache lookup time: %q", time.Since(startTime))
	}()

	filename := cacheFilename(makefile, roots)
	if !exists(filename) {
		LogAlways("Cache not found")
		return nil
	}

	g := LoadDepGraph(filename)
	for _, mk := range g.readMks {
		if mk.State != FileExists && mk.State != FileNotExists {
			panic(fmt.Sprintf("Internal error: broken state: %d", mk.State))
		}
		if mk.State == FileNotExists {
			if exists(mk.Filename) {
				LogAlways("Cache expired: %s", mk.Filename)
				return nil
			}
		} else {
			c, err := ioutil.ReadFile(mk.Filename)
			if err != nil {
				LogAlways("Cache expired: %s", mk.Filename)
				return nil
			}
			h := sha1.Sum(c)
			if !bytes.Equal(h[:], mk.Hash[:]) {
				LogAlways("Cache expired: %s", mk.Filename)
				return nil
			}
		}
	}
	g.isCached = true
	LogAlways("Cache found!")
	return g
}
