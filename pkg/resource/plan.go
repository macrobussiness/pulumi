// Copyright 2016 Marapongo, Inc. All rights reserved.

package resource

import (
	"github.com/golang/glog"

	"github.com/marapongo/mu/pkg/graph"
	"github.com/marapongo/mu/pkg/util/contract"
)

// TODO: concurrency.
// TODO: handle output dependencies

// Plan is the output of analyzing resource graphs and contains the steps necessary to perform an infrastructure
// deployment.  A plan can be generated out of whole cloth from a resource graph -- in the case of new deployments --
// however, it can alternatively be generated by diffing two resource graphs -- in the case of updates to existing
// environments (presumably more common).  The plan contains step objects that can be used to drive a deployment.
type Plan interface {
	Empty() bool                                      // true if the plan is empty.
	Steps() Step                                      // the first step to perform, linked to the rest.
	Apply(prog Progress) (error, Step, ResourceState) // performs the operations specified in this plan.
}

// Progress can be used for progress reporting.
type Progress interface {
	Before(step Step)
	After(step Step, err error, state ResourceState)
}

// Step is a specification for a deployment operation.
type Step interface {
	Op() StepOp                    // the operation that will be performed.
	Old() Resource                 // the old resource state, if any, before performing this step.
	New() Resource                 // the new resource state, if any, after performing this step.
	Next() Step                    // the next step to perform, or nil if none.
	Apply() (error, ResourceState) // performs the operation specified by this step.
}

// StepOp represents the kind of operation performed by this step.
type StepOp int

const (
	OpCreate StepOp = iota
	OpRead
	OpUpdate
	OpDelete
)

// NewCreatePlan creates a plan for instantiating a new snapshot from scratch.
func NewCreatePlan(ctx *Context, s Snapshot) Plan {
	contract.Requiref(s != nil, "s", "!= nil")
	return newCreatePlan(ctx, s)
}

// NewDeletePlan creates a plan for deleting an entire snapshot in its entirety.
func NewDeletePlan(ctx *Context, s Snapshot) Plan {
	contract.Requiref(s != nil, "s", "!= nil")
	return newDeletePlan(ctx, s)
}

// NewUpdatePlan analyzes a a resource graph rg compared to an optional old resource graph oldRg, and creates a plan
// that will carry out operations necessary to bring the old resource graph in line with the new one.
func NewUpdatePlan(ctx *Context, s Snapshot, old Snapshot) Plan {
	contract.Requiref(s != nil, "s", "!= nil")
	contract.Requiref(old != nil, "old", "!= nil")
	return newUpdatePlan(ctx, s, old)
}

type plan struct {
	ctx   *Context // this plan's context.
	first *step    // the first step to take.
}

var _ Plan = (*plan)(nil)

func (p *plan) Empty() bool { return p.Steps() == nil }

func (p *plan) Steps() Step {
	if p.first == nil {
		return nil
	}
	return p.first
}

// Provider fetches the provider for a given resource, possibly lazily allocating the plugins for it.  If a provider
// could not be found, or an error occurred while creating it, a non-nil error is returned.
func (p *plan) Provider(res Resource) (Provider, error) {
	t := res.Type()
	pkg := t.Package()
	return p.ctx.Provider(pkg)
}

// Apply performs all steps in the plan, calling out to the progress reporting functions as desired.
func (p *plan) Apply(prog Progress) (error, Step, ResourceState) {
	var step Step = p.first
	for step != nil {
		if prog != nil {
			prog.Before(step)
		}
		err, rst := step.Apply()
		if prog != nil {
			prog.After(step, err, rst)
		}
		if err != nil {
			return err, step, rst
		}
		step = step.Next()
	}
	return nil, nil, StateOK
}

func newCreatePlan(ctx *Context, s Snapshot) *plan {
	if glog.V(7) {
		glog.V(7).Infof("Creating create plan with #s=%v\n", len(s.Resources()))
	}

	// To create the resources, we must perform the operations in dependency order.  That is, we create the leaf-most
	// resource first, so that later resources may safely depend upon their dependencies having been created.
	p := &plan{ctx: ctx}
	var prev *step
	for _, res := range s.Resources() {
		step := newCreateStep(p, res)
		insertStep(&prev, step)
	}
	return p
}

func newDeletePlan(ctx *Context, s Snapshot) *plan {
	if glog.V(7) {
		glog.V(7).Infof("Creating delete plan with #s=%v\n", len(s.Resources()))
	}

	// To delete an entire snapshot, we must perform the operations in reverse dependency order.  That is, resources
	// that consume others should be deleted first, so dependencies do not get deleted "out from underneath" consumers.
	p := &plan{ctx: ctx}
	var prev *step
	resources := s.Resources()
	for i := len(resources) - 1; i >= 0; i-- {
		res := resources[i]
		step := newDeleteStep(p, res)
		insertStep(&prev, step)
	}
	return p
}

func newUpdatePlan(ctx *Context, old Snapshot, new Snapshot) *plan {
	if glog.V(7) {
		glog.V(7).Infof("Creating update plan with #old=%v #new=%v\n", len(old.Resources()), len(new.Resources()))
	}

	// First diff the snapshots; in a nutshell:
	//
	//     - Anything in old but not new is a delete
	//     - Anything in new but not old is a create
	//     - For those things in both new and old, any changed properties imply an update
	//
	// There are some caveats:
	//
	//     - Any changes in dependencies are possibly interesting
	//     - Any changes in moniker are interesting (see note on stability in monikers.go)
	//
	olds := make(map[Moniker]Resource)
	olddepends := make(map[Moniker][]Moniker)
	for _, res := range old.Resources() {
		m := res.Moniker()
		olds[m] = res
		// Keep track of which dependents exist for all resources.
		for ref := range res.Properties().AllResources() {
			olddepends[ref] = append(olddepends[ref], m)
		}
	}
	news := make(map[Moniker]Resource)
	for _, res := range new.Resources() {
		news[res.Moniker()] = res
	}

	// Keep track of vertices for our later graph operations.
	p := &plan{ctx: ctx}
	vs := make(map[Moniker]*planVertex)

	// Find those things in old but not new, and add them to the delete queue.
	deletes := make(map[Resource]bool)
	for _, res := range olds {
		m := res.Moniker()
		if _, has := news[m]; !has {
			deletes[res] = true
			step := newDeleteStep(p, res)
			vs[m] = newPlanVertex(step)
			glog.V(7).Infof("Update plan decided to delete '%v'", m)
		}
	}

	// Find creates and updates: creates are those in new but not old, and updates are those in both.
	creates := make(map[Resource]bool)
	updates := make(map[Resource]Resource)
	for _, res := range news {
		m := res.Moniker()
		if old, has := olds[m]; has {
			contract.Assert(old.Type() == res.Type())
			if !res.Properties().DeepEquals(old.Properties()) {
				updates[old] = res
				step := newUpdateStep(p, old, res)
				vs[m] = newPlanVertex(step)
				glog.V(7).Infof("Update plan decided to update '%v'", m)
			} else if glog.V(7) {
				glog.V(7).Infof("Update plan decided not to update '%v'", m)
			}
		} else {
			creates[res] = true
			step := newCreateStep(p, res)
			vs[m] = newPlanVertex(step)
			glog.V(7).Infof("Update plan decided to create '%v'", m)
		}
	}

	// Finally, we need to sequence the overall set of changes to create the final plan.  To do this, we create a DAG
	// of the above operations, so that inherent dependencies between operations are respected; specifically:
	//
	//     - Deleting a resource depends on deletes of dependents and updates whose olds refer to it
	//     - Creating a resource depends on creates of dependencies
	//     - Updating a resource depends on creates or updates of news
	//
	// Clearly we must prohibit cycles in this overall graph of resource operations (hence the DAG part).  To ensure
	// this ordering, we will produce a plan graph whose vertices are operations and whose edges encode dependencies.
	for _, res := range old.Resources() {
		m := res.Moniker()
		if deletes[res] {
			// Add edges to:
			//     - any dependents that used to refer to this
			tov := vs[m]
			contract.Assert(tov != nil)
			for _, ref := range olddepends[m] {
				fromv := vs[ref]
				contract.Assert(fromv != nil)
				fromv.connectTo(tov)
				glog.V(7).Infof("Deletion '%v' depends on resource '%v'", m, ref)
			}
		} else if to := updates[res]; to != nil {
			// Add edge to:
			//     - creates news
			//     - updates news
			// TODO[marapongo/mu#90]: we need to track "cascading updates".
			fromv := vs[m]
			contract.Assert(fromv != nil)
			for ref := range to.Properties().AllResources() {
				tov := vs[ref]
				contract.Assert(tov != nil)
				fromv.connectTo(tov)
				glog.V(7).Infof("Updating '%v' depends on resource '%v'", m, ref)
			}
		}
	}
	for _, res := range new.Resources() {
		if creates[res] {
			// add edge to:
			//     - creates news
			m := res.Moniker()
			fromv := vs[m]
			contract.Assert(fromv != nil)
			for ref := range res.Properties().AllResources() {
				tov := vs[ref]
				contract.Assert(tov != nil)
				fromv.connectTo(tov)
				glog.V(7).Infof("Creating '%v' depends on resource '%v'", m, ref)
			}
		}
	}

	// For all vertices with no ins, make them root nodes.
	var roots []*planEdge
	for _, v := range vs {
		if len(v.Ins()) == 0 {
			roots = append(roots, &planEdge{to: v})
		}
	}

	// Now topologically sort the steps, thread the plan together, and return it.
	g := newPlanGraph(p, roots)
	topdag, err := graph.Topsort(g)
	contract.Assertf(err == nil, "Unexpected error topologically sorting update plan")
	var prev *step
	for _, v := range topdag {
		insertStep(&prev, v.Data().(*step))
	}
	return p
}

type step struct {
	p    *plan    // this step's plan.
	op   StepOp   // the operation to perform.
	old  Resource // the state of the resource before this step.
	new  Resource // the state of the resource after this step.
	next *step    // the next step after this one in the plan.
}

var _ Step = (*step)(nil)

func (s *step) Op() StepOp    { return s.op }
func (s *step) Old() Resource { return s.old }
func (s *step) New() Resource { return s.new }
func (s *step) Next() Step {
	if s.next == nil {
		return nil
	}
	return s.next
}

func newCreateStep(p *plan, new Resource) *step {
	return &step{p: p, op: OpCreate, new: new}
}

func newDeleteStep(p *plan, old Resource) *step {
	return &step{p: p, op: OpDelete, old: old}
}

func newUpdateStep(p *plan, old Resource, new Resource) *step {
	return &step{p: p, op: OpUpdate, old: old, new: new}
}

func insertStep(prev **step, step *step) {
	contract.Assert(prev != nil)
	if *prev == nil {
		contract.Assert(step.p.first == nil)
		step.p.first = step
		*prev = step
	} else {
		(*prev).next = step
		*prev = step
	}
}

func (s *step) Apply() (error, ResourceState) {
	// Now simply perform the operation of the right kind.
	switch s.op {
	case OpCreate:
		contract.Assert(s.old == nil)
		contract.Assert(s.new != nil)
		contract.Assertf(!s.new.HasID(), "Resources being created must not have IDs already")
		prov, err := s.p.Provider(s.new)
		if err != nil {
			return err, StateOK
		}
		id, err, rst := prov.Create(s.new.Type(), s.new.Properties())
		if err != nil {
			return err, rst
		}
		s.new.SetID(id)
	case OpDelete:
		contract.Assert(s.old != nil)
		contract.Assert(s.new == nil)
		contract.Assertf(s.old.HasID(), "Resources being deleted must have IDs")
		prov, err := s.p.Provider(s.old)
		if err != nil {
			return err, StateOK
		}
		if err, rst := prov.Delete(s.old.ID(), s.old.Type()); err != nil {
			return err, rst
		}
	case OpUpdate:
		contract.Assert(s.old != nil)
		contract.Assert(s.new != nil)
		contract.Assert(s.old.Type() == s.new.Type())
		contract.Assertf(s.old.HasID(), "Resources being updated must have IDs")
		prov, err := s.p.Provider(s.old)
		if err != nil {
			return err, StateOK
		}
		id, err, rst := prov.Update(s.old.ID(), s.old.Type(), s.old.Properties(), s.new.Properties())
		if err != nil {
			return err, rst
		} else if id != ID("") {
			// An update might need to recreate the resource, in which case the ID must change.
			// TODO: this could have an impact on subsequent dependent resources that wasn't known during planning.
			s.new.SetID(id)
		}
	default:
		contract.Failf("Unexpected step operation: %v", s.op)
	}

	return nil, StateOK
}
