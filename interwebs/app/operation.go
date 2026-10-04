package app

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// Operation is profile-owned work. Canceling Wait does not cancel the operation.
type Operation struct {
	mu         sync.Mutex
	id, kind   string
	site       *Site
	background bool
	cancel     context.CancelFunc
	done       chan struct{}
	status     OperationStatus
	result     Result
	err        error
}

func (o *Operation) ID() string { return o.id }
func (o *Operation) Cancel()    { o.cancel() }
func (o *Operation) Status() OperationStatus {
	o.mu.Lock()
	status := o.status
	o.mu.Unlock()
	if status.State == Running {
		if n := o.site.currentNode(); n != nil {
			progress := n.Status()
			status.Bytes, status.Total = progress.Bytes, progress.Total
		}
	}
	return status
}

// Operations returns queued/running work and the most recent 128 completed operations.
func (p *Profile) Operations() []OperationStatus {
	p.mu.Lock()
	operations := make([]*Operation, 0, len(p.operations))
	for _, operation := range p.operations {
		operations = append(operations, operation)
	}
	p.mu.Unlock()
	result := make([]OperationStatus, 0, len(operations))
	for _, operation := range operations {
		result = append(result, operation.Status())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Wait returns a copied result; canceling ctx only stops waiting. A returned View is shared
// between waiters and must be closed by its owner; repeated View.Close calls are safe.
func (o *Operation) Wait(ctx context.Context) (Result, error) {
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-o.done:
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	result := o.result
	if result.Publication != nil {
		copy := *result.Publication
		copy.Record = copy.Record.Clone()
		result.Publication = &copy
	}
	if result.Refresh != nil {
		copy := *result.Refresh
		result.Refresh = &copy
	}
	return result, o.err
}

func (p *Profile) Operation(id string) (*Operation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	op := p.operations[id]
	if op == nil {
		return nil, ErrNotFound
	}
	return op, nil
}

func (s *Site) submit(ctx context.Context, kind string, background bool, fn func(context.Context) (Result, error)) (*Operation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	p := s.profile
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.alive(); err != nil {
		return nil, err
	}
	if p.sites[s.id] != s {
		return nil, ErrNotFound
	}
	if p.state.Paused && (background || kind == "publish") {
		return nil, ErrPaused
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	work, cancel := context.WithCancel(context.Background())
	op := &Operation{id: id, kind: kind, site: s, background: background, cancel: cancel, done: make(chan struct{}),
		status: OperationStatus{ID: id, SiteID: s.id, Kind: kind, State: Queued}}
	if !background && s.background != nil {
		s.background.Cancel()
	}
	if background {
		s.background = op
	} else {
		s.foreground++
	}
	s.pending++
	p.operations[id] = op
	p.wg.Add(1)
	go op.run(work, fn)
	p.notifyLocked(Change{SiteID: s.id, OperationID: id})
	return op, nil
}

func (o *Operation) run(ctx context.Context, fn func(context.Context) (Result, error)) {
	p, s := o.site.profile, o.site
	defer p.wg.Done()
	defer o.cancel()
	var result Result
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case p.workers <- struct{}{}:
			defer func() { <-p.workers }()
			p.mu.Lock()
			paused := p.state.Paused && (o.background || o.kind == "publish")
			p.mu.Unlock()
			if paused {
				err = ErrPaused
			} else if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				o.mu.Lock()
				o.status.State, o.status.Started = Running, p.app.now()
				o.mu.Unlock()
				result, err = fn(ctx)
			}
		}
	}
	o.mu.Lock()
	o.result, o.err = result, err
	o.status.State = Succeeded
	if err != nil {
		o.status.State = Failed
		o.status.Error = err.Error()
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrPaused) {
			o.status.State = Canceled
		}
	}
	o.status.Finished = p.app.now()
	o.mu.Unlock()
	p.mu.Lock()
	s.pending--
	if o.background {
		if s.background == o {
			s.background = nil
		}
	} else {
		s.foreground--
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrPaused) {
		s.lastError = err.Error()
	} else if err == nil {
		s.lastError = ""
	}
	p.completed = append(p.completed, o.id)
	if len(p.completed) > 128 {
		delete(p.operations, p.completed[0])
		p.completed = p.completed[1:]
	}
	p.notifyLocked(Change{SiteID: s.id, OperationID: o.id})
	p.mu.Unlock()
	<-p.slots
	close(o.done)
}
