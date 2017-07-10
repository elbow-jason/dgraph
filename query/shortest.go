/*
 * Copyright (C) 2017 Dgraph Labs, Inc. and Contributors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package query

import (
	"container/heap"
	"context"

	"golang.org/x/net/trace"

	"github.com/dgraph-io/dgraph/algo"
	"github.com/dgraph-io/dgraph/protos"
	"github.com/dgraph-io/dgraph/x"
)

func (start *SubGraph) expandOut(ctx context.Context,
	adjacencyMap map[uint64]map[uint64]mapItem, next chan bool, rch chan error) {

	var numEdges uint64
	var exec []*SubGraph
	var err error
	in := []uint64{start.Params.From}
	start.SrcUIDs = &protos.List{in}
	start.uidMatrix = []*protos.List{{in}}
	start.DestUIDs = start.SrcUIDs

	for _, child := range start.Children {
		child.SrcUIDs = start.DestUIDs
		exec = append(exec, child)
	}
	dummy := &SubGraph{}
	for {
		isNext := <-next
		if !isNext {
			return
		}
		rrch := make(chan error, len(exec))
		for _, sg := range exec {
			go ProcessGraph(ctx, sg, dummy, rrch)
		}

		for range exec {
			select {
			case err = <-rrch:
				if err != nil {
					if tr, ok := trace.FromContext(ctx); ok {
						tr.LazyPrintf("Error while processing child task: %+v", err)
					}
					rch <- err
					return
				}
			case <-ctx.Done():
				if tr, ok := trace.FromContext(ctx); ok {
					tr.LazyPrintf("Context done before full execution: %+v", ctx.Err())
				}
				rch <- ctx.Err()
				return
			}
		}

		for _, sg := range exec {
			// Send the destuids in res chan.
			for mIdx, fromUID := range sg.SrcUIDs.Uids {
				for lIdx, toUID := range sg.uidMatrix[mIdx].Uids {
					if adjacencyMap[fromUID] == nil {
						adjacencyMap[fromUID] = make(map[uint64]mapItem)
					}
					// The default cost we'd use is 1.
					cost, facet, err := sg.getCost(mIdx, lIdx)
					if err == ErrFacet {
						// Ignore the edge and continue.
						continue
					} else if err != nil {
						rch <- err
						return
					}
					adjacencyMap[fromUID][toUID] = mapItem{
						cost:  cost,
						facet: facet,
						attr:  sg.Attr,
					}
					numEdges++
				}
			}
		}

		if numEdges > 10000000 {
			// If we've seen too many nodes, stop the query.
			rch <- ErrTooBig
			return
		}

		// modify the exec and attach child nodes.
		var out []*SubGraph
		for _, sg := range exec {
			if len(sg.DestUIDs.Uids) == 0 {
				continue
			}
			for _, child := range start.Children {
				temp := new(SubGraph)
				temp.copyFiltersRecurse(child)

				temp.SrcUIDs = sg.DestUIDs
				// Remove those nodes which we have already traversed. As this cannot be
				// in the path again.
				algo.ApplyFilter(temp.SrcUIDs, func(uid uint64, i int) bool {
					_, ok := adjacencyMap[uid]
					return !ok
				})
				if len(temp.SrcUIDs.Uids) == 0 {
					continue
				}
				sg.Children = append(sg.Children, temp)
				out = append(out, temp)
			}
		}

		if len(out) == 0 {
			rch <- ErrStop
			return
		}
		rch <- nil
		exec = out
	}
}

func (temp *SubGraph) copyFiltersRecurse(sg *SubGraph) {
	*temp = *sg
	temp.Children = []*SubGraph{}
	temp.Filters = []*SubGraph{}
	for _, fc := range sg.Filters {
		tempChild := new(SubGraph)
		tempChild.copyFiltersRecurse(fc)
		temp.Filters = append(temp.Filters, tempChild)
	}
}

// Djikstras algorithm pseudocode for reference.
//
//
// 1  function Dijkstra(Graph, source):
// 2      dist[source] ← 0                                    // Initialization
// 3
// 4      create vertex set Q
// 5
// 6      for each vertex v in Graph:
// 7          if v ≠ source
// 8              dist[v] ← INFINITY                          // Unknown distance from source to v
// 9              prev[v] ← UNDEFINED                         // Predecessor of v
// 10
// 11         Q.add_with_priority(v, dist[v])
// 12
// 13
// 14     while Q is not empty:                              // The main loop
// 15         u ← Q.extract_min()                            // Remove and return best vertex
// 16         for each neighbor v of u:                       // only v that is still in Q
// 17             alt = dist[u] + length(u, v)
// 18             if alt < dist[v]
// 19                 dist[v] ← alt
// 20                 prev[v] ← u
// 21                 Q.decrease_priority(v, alt)
// 22
// 23     return dist[], prev[]

func ShortestPath(ctx context.Context, sg *SubGraph) ([]*SubGraph, error) {
	var err error
	if sg.Params.Alias != "shortest" {
		return nil, x.Errorf("Invalid shortest path query")
	}
	numPaths := sg.Params.numPaths
	if numPaths == 0 {
		// Return 1 path by default.
		numPaths = 1
	}

	if numPaths > 1 {
		return KShortestPath(ctx, sg)
	}
	pq := make(priorityQueue, 0)
	heap.Init(&pq)

	// Initialize and push the source node.
	srcNode := &Item{
		uid:  sg.Params.From,
		cost: 0,
		hop:  0,
	}
	heap.Push(&pq, srcNode)

	numHops := -1
	next := make(chan bool, 2)
	expandErr := make(chan error, 2)
	adjacencyMap := make(map[uint64]map[uint64]mapItem)
	go sg.expandOut(ctx, adjacencyMap, next, expandErr)

	// map to store the min cost and parent of nodes.
	dist := make(map[uint64]nodeInfo)
	dist[srcNode.uid] = nodeInfo{
		parent: 0,
		node:   srcNode,
		mapItem: mapItem{
			cost: 0,
		},
	}

	var stopExpansion bool
	for pq.Len() > 0 {
		item := heap.Pop(&pq).(*Item)
		if item.uid == sg.Params.To {
			break
		}
		if item.hop > numHops {
			// Explore the next level by calling processGraph and add them
			// to the queue.
			if !stopExpansion {
				next <- true
			}
			select {
			case err = <-expandErr:
				if err != nil {
					if err == ErrTooBig {
						return nil, err
					} else if err == ErrStop {
						stopExpansion = true
					} else {
						if tr, ok := trace.FromContext(ctx); ok {
							tr.LazyPrintf("Error while processing child task: %+v", err)
						}
						return nil, err
					}
				}
			case <-ctx.Done():
				if tr, ok := trace.FromContext(ctx); ok {
					tr.LazyPrintf("Context done before full execution: %+v", ctx.Err())
				}
				return nil, ctx.Err()
			}
			numHops++
		}
		if !stopExpansion {
			neighbours := adjacencyMap[item.uid]
			for toUid, info := range neighbours {
				cost := info.cost
				d, ok := dist[toUid]
				if ok && d.cost <= item.cost+cost {
					continue
				}
				if !ok {
					// This is the first time we're seeing this node. So
					// create a new node and add it to the heap and map.
					node := &Item{
						uid:  toUid,
						cost: item.cost + cost,
						hop:  item.hop + 1,
					}
					heap.Push(&pq, node)
					dist[toUid] = nodeInfo{
						parent: item.uid,
						node:   node,
						mapItem: mapItem{
							cost:  item.cost + cost,
							attr:  info.attr,
							facet: info.facet,
						},
					}
				} else {
					// We've already seen this node. So, just update the cost
					// and fix the priority in the heap and map.
					node := dist[toUid].node
					node.cost = item.cost + cost
					node.hop = item.hop + 1
					heap.Fix(&pq, node.index)
					// Update the map with new values.
					dist[toUid] = nodeInfo{
						parent: item.uid,
						node:   node,
						mapItem: mapItem{
							cost:  item.cost + cost,
							attr:  info.attr,
							facet: info.facet,
						},
					}
				}

			}
		}
	}

	next <- false
	// Go through the distance map to find the path.
	var result []uint64
	cur := sg.Params.To
	for i := 0; cur != sg.Params.From && i < len(dist); i++ {
		result = append(result, cur)
		cur = dist[cur].parent
	}
	// Put the path in DestUIDs of the root.
	if cur != sg.Params.From {
		sg.DestUIDs = &protos.List{}
		return nil, nil
	}

	result = append(result, cur)
	l := len(result)
	// Reverse the list.
	for i := 0; i < l/2; i++ {
		result[i], result[l-i-1] = result[l-i-1], result[i]
	}
	sg.DestUIDs.Uids = result

	shortestSg := createPathSubgraph(ctx, dist, result)
	return []*SubGraph{shortestSg}, nil
}

func createPathSubgraph(ctx context.Context, dist map[uint64]nodeInfo, result []uint64) *SubGraph {
	shortestSg := new(SubGraph)
	shortestSg.Params = params{
		Alias:  "_path_",
		GetUid: true,
	}
	curUid := result[0]
	shortestSg.SrcUIDs = &protos.List{[]uint64{curUid}}
	shortestSg.DestUIDs = &protos.List{[]uint64{curUid}}
	shortestSg.uidMatrix = []*protos.List{{[]uint64{curUid}}}

	curNode := shortestSg
	for i := 0; i < len(result)-1; i++ {
		curUid := result[i]
		childUid := result[i+1]
		node := new(SubGraph)
		nodeInfo := dist[childUid]
		node.Params = params{
			GetUid: true,
		}
		if nodeInfo.facet != nil {
			// For consistent later processing.
			node.Params.Facet = &protos.Param{}
		}
		node.Attr = nodeInfo.attr
		node.facetsMatrix = []*protos.FacetsList{{[]*protos.Facets{nodeInfo.facet}}}
		node.SrcUIDs = &protos.List{[]uint64{curUid}}
		node.DestUIDs = &protos.List{[]uint64{childUid}}
		node.uidMatrix = []*protos.List{{[]uint64{childUid}}}

		curNode.Children = append(curNode.Children, node)
		curNode = node
	}

	node := new(SubGraph)
	node.Params = params{
		GetUid: true,
	}
	uid := result[len(result)-1]
	node.SrcUIDs = &protos.List{[]uint64{uid}}
	node.uidMatrix = []*protos.List{{[]uint64{uid}}}
	curNode.Children = append(curNode.Children, node)

	return shortestSg
}
