// +build linux

package iouring

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	r, err := New(2048, nil)
	require.NoError(t, err)
	require.NotNil(t, r)

	require.NotZero(t, r.Sq.Size)
	require.NotNil(t, r.Sq.Head)
	require.NotNil(t, r.Sq.Tail)
	require.NotNil(t, r.Sq.Mask)
	require.NotNil(t, r.Sq.Entries)
	require.NotNil(t, r.Sq.Flags)
	require.NotNil(t, r.Sq.Dropped)
	require.NotNil(t, r.Sq.Entries)

	require.NotZero(t, r.Cq.Size)
	require.NotNil(t, r.Cq.Head)
	require.NotNil(t, r.Cq.Tail)
	require.NotNil(t, r.Cq.Mask)
	require.NotNil(t, r.Cq.Entries)

	require.NoError(t, r.Close())
}

func TestNewRingInvalidSize(t *testing.T) {
	_, err := New(99999, nil)
	require.Error(t, err)
}

func TestRingEnter(t *testing.T) {
	r, err := New(2048, nil)
	require.NoError(t, err)
	require.NotNil(t, r)
	count := 0
	for i := r.SubmitHead(); i < r.SubmitTail(); i++ {
		r.Sq.Entries[i] = SubmitEntry{
			Opcode:   Nop,
			UserData: uint64(i),
		}
		count++
	}
	err = r.Enter(uint(count), uint(count), EnterGetEvents, nil)
	require.NoError(t, err)
}
