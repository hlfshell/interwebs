package interwebs

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"

	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/identity"
	"github.com/hlfshell/interweb/interwebs/internal/profile"
)

func loadState(ctx context.Context, p *profile.Profile, c content.Content, signer *identity.Signer) (nodeState, content.Source, error) {
	descriptor := c.Descriptor()
	state := nodeState{
		Version:  4,
		Name:     descriptor.Name,
		SourceID: descriptor.SourceID,
		Identity: descriptor.Address,
		History:  make([]identity.Record, 0),
	}
	encoded, err := p.ReadState(ctx)
	fresh := errors.Is(err, fs.ErrNotExist)
	if err != nil && !fresh {
		return nodeState{}, nil, err
	}
	if !fresh {
		state = nodeState{}
		if err = json.Unmarshal(encoded, &state); err != nil {
			return nodeState{}, nil, err
		}
		if state.Version != 4 {
			return nodeState{}, nil, errors.New("unsupported node state version")
		}
		if descriptor.Address != (identity.Identity{}) && descriptor.Address != state.Identity {
			return nodeState{}, nil, errors.New("content identity does not match node directory")
		}
		if state.Name != descriptor.Name && descriptor.Address == (identity.Identity{}) {
			return nodeState{}, nil, errors.New("source does not match node directory")
		}
		if state.SourceID != descriptor.SourceID && descriptor.Address == (identity.Identity{}) {
			return nodeState{}, nil, errors.New("source identity does not match node directory")
		}
	}

	// A local source needs explicit signing authority; remote references do not.
	var source content.Source
	if descriptor.Address == (identity.Identity{}) {
		source, _ = c.(content.Source)
		if signer == nil {
			return nodeState{}, nil, errors.New("new content requires explicit signing authority")
		}
		if !fresh && state.Identity != signer.Identity() {
			return nodeState{}, nil, errors.New("signer does not match saved identity")
		}
		state.Identity = signer.Identity()
	}
	if err := state.Identity.Validate(); err != nil {
		return nodeState{}, nil, err
	}
	if signer != nil && signer.Identity() != state.Identity {
		return nodeState{}, nil, errors.New("signer does not match content identity")
	}
	if state.Record.Hash != "" && state.Identity.Key != "" {
		if err := state.Identity.Verify(state.Record, identity.Record{}); err != nil {
			return nodeState{}, nil, err
		}
	}

	return state, source, nil
}
