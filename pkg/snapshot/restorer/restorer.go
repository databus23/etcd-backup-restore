// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package restorer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/embed"
	"github.com/coreos/etcd/etcdserver"
	"github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/coreos/etcd/etcdserver/membership"
	"github.com/coreos/etcd/lease"
	"github.com/coreos/etcd/mvcc"
	"github.com/coreos/etcd/mvcc/backend"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/coreos/etcd/pkg/fileutil"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/coreos/etcd/snap"
	"github.com/coreos/etcd/store"
	"github.com/coreos/etcd/wal"
	"github.com/coreos/etcd/wal/walpb"
	"github.com/gardener/etcd-backup-restore/pkg/snapstore"
	"github.com/sirupsen/logrus"
)

// NewRestorer returns the snapshotter object.
func NewRestorer(store snapstore.SnapStore, logger *logrus.Logger) *Restorer {
	return &Restorer{
		logger: logger,
		store:  store,
	}
}

// Restore restore the etcd data directory as per specified restore options
func (r *Restorer) Restore(ro RestoreOptions) error {
	if err := r.restoreFromBaseSnapshot(ro); err != nil {
		return fmt.Errorf("failed to restore from the base snapshot :%v", err)
	}

	r.logger.Infof("Starting embedded etcd server...")
	e, err := startEmbeddedEtcd(ro)
	if err != nil {
		return err
	}
	defer e.Close()

	client, err := clientv3.NewFromURL(e.Clients[0].Addr().String())
	if err != nil {
		return err
	}
	defer client.Close()
	r.logger.Infof("Applying incremental snapshots...")
	return r.applyDeltaSnapshots(client, ro.DeltaSnapList)
}

// restoreFromBaseSnapshot restore the etcd data directory from base snapshot
func (r *Restorer) restoreFromBaseSnapshot(ro RestoreOptions) error {
	var err error
	if path.Join(ro.BaseSnapshot.SnapDir, ro.BaseSnapshot.SnapName) == "" {
		r.logger.Warnf("Base snapshot path not provided. Will do nothing.")
		return nil
	}

	cfg := etcdserver.ServerConfig{
		InitialClusterToken: ro.ClusterToken,
		InitialPeerURLsMap:  ro.ClusterURLs,
		PeerURLs:            ro.PeerURLs,
		Name:                ro.Name,
	}
	if err := cfg.VerifyBootstrap(); err != nil {
		return err
	}

	cl, err := membership.NewClusterFromURLsMap(ro.ClusterToken, ro.ClusterURLs)
	if err != nil {
		return err
	}

	memberDir := filepath.Join(ro.RestoreDataDir, "member")
	if _, err := os.Stat(memberDir); err == nil {
		return fmt.Errorf("member directory in data directory(%q) exists", memberDir)
	}

	walDir := filepath.Join(ro.RestoreDataDir, "member", "wal")
	snapdir := filepath.Join(ro.RestoreDataDir, "member", "snap")
	if err = makeDB(snapdir, ro.BaseSnapshot, len(cl.Members()), r.store, false); err != nil {
		return err
	}
	return makeWALAndSnap(walDir, snapdir, cl, ro.Name)
}

// makeDB copies the database snapshot to the snapshot directory
func makeDB(snapdir string, snap snapstore.Snapshot, commit int, ss snapstore.SnapStore, skipHashCheck bool) error {
	rc, err := ss.Fetch(snap)
	if err != nil {
		return err
	}
	defer rc.Close()

	if err := fileutil.CreateDirAll(snapdir); err != nil {
		return err
	}

	dbPath := filepath.Join(snapdir, "db")
	db, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(db, rc); err != nil {
		return err
	}
	db.Sync()

	off, err := db.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	hasHash := (off % 512) == sha256.Size
	if !hasHash && !skipHashCheck {
		err := fmt.Errorf("snapshot missing hash but --skip-hash-check=false")
		return err
	}

	if hasHash {
		// get snapshot integrity hash
		if _, err = db.Seek(-sha256.Size, io.SeekEnd); err != nil {
			return err
		}
		sha := make([]byte, sha256.Size)
		if _, err := db.Read(sha); err != nil {
			return err
		}
		// truncate away integrity hash
		if err = db.Truncate(off - sha256.Size); err != nil {
			return err
		}

		if !skipHashCheck {
			if _, err := db.Seek(0, io.SeekStart); err != nil {
				return err
			}
			// check for match
			h := sha256.New()
			if _, err = io.Copy(h, db); err != nil {
				return err
			}
			dbSha := h.Sum(nil)
			if !reflect.DeepEqual(sha, dbSha) {
				err := fmt.Errorf("expected sha256 %v, got %v", sha, dbSha)
				return err
			}
		}
	}

	// db hash is OK
	db.Close()
	// update consistentIndex so applies go through on etcdserver despite
	// having a new raft instance
	be := backend.NewDefaultBackend(dbPath)
	// a lessor never timeouts leases
	lessor := lease.NewLessor(be, math.MaxInt64)
	s := mvcc.NewStore(be, lessor, (*initIndex)(&commit))
	txn := s.Write()
	btx := be.BatchTx()
	del := func(k, v []byte) error {
		txn.DeleteRange(k, nil)
		return nil
	}

	// delete stored members from old cluster since using new members
	btx.UnsafeForEach([]byte("members"), del)
	// todo: add back new members when we start to deprecate old snap file.
	btx.UnsafeForEach([]byte("members_removed"), del)
	// trigger write-out of new consistent index
	txn.End()
	s.Commit()
	s.Close()
	be.Close()
	return nil
}

func makeWALAndSnap(waldir, snapdir string, cl *membership.RaftCluster, restoreName string) error {
	if err := fileutil.CreateDirAll(waldir); err != nil {
		return err
	}

	// add members again to persist them to the store we create.
	st := store.New(etcdserver.StoreClusterPrefix, etcdserver.StoreKeysPrefix)
	cl.SetStore(st)
	for _, m := range cl.Members() {
		cl.AddMember(m)
	}

	m := cl.MemberByName(restoreName)
	md := &etcdserverpb.Metadata{NodeID: uint64(m.ID), ClusterID: uint64(cl.ID())}
	metadata, err := md.Marshal()
	if err != nil {
		return err
	}

	w, err := wal.Create(waldir, metadata)
	if err != nil {
		return err
	}
	defer w.Close()

	peers := make([]raft.Peer, len(cl.MemberIDs()))
	for i, id := range cl.MemberIDs() {
		ctx, err := json.Marshal((*cl).Member(id))
		if err != nil {
			return err
		}
		peers[i] = raft.Peer{ID: uint64(id), Context: ctx}
	}

	ents := make([]raftpb.Entry, len(peers))
	nodeIDs := make([]uint64, len(peers))
	for i, p := range peers {
		nodeIDs[i] = p.ID
		cc := raftpb.ConfChange{
			Type:    raftpb.ConfChangeAddNode,
			NodeID:  p.ID,
			Context: p.Context}
		d, err := cc.Marshal()
		if err != nil {
			return err
		}
		e := raftpb.Entry{
			Type:  raftpb.EntryConfChange,
			Term:  1,
			Index: uint64(i + 1),
			Data:  d,
		}
		ents[i] = e
	}

	commit, term := uint64(len(ents)), uint64(1)

	if err := w.Save(raftpb.HardState{
		Term:   term,
		Vote:   peers[0].ID,
		Commit: commit}, ents); err != nil {
		return err
	}

	b, err := st.Save()
	if err != nil {
		return err
	}

	raftSnap := raftpb.Snapshot{
		Data: b,
		Metadata: raftpb.SnapshotMetadata{
			Index: commit,
			Term:  term,
			ConfState: raftpb.ConfState{
				Nodes: nodeIDs,
			},
		},
	}
	snapshotter := snap.New(snapdir)
	if err := snapshotter.SaveSnap(raftSnap); err != nil {
		panic(err)
	}

	return w.SaveSnapshot(walpb.Snapshot{Index: commit, Term: term})
}

// startEmbeddedEtcd starts the embedded etcd server
func startEmbeddedEtcd(ro RestoreOptions) (*embed.Etcd, error) {
	cfg := embed.NewConfig()
	cfg.Dir = ro.RestoreDataDir
	e, err := embed.StartEtcd(cfg)
	if err != nil {
		return nil, err
	}
	select {
	case <-e.Server.ReadyNotify():
		fmt.Printf("Embedded server is ready!\n")
	case <-time.After(60 * time.Second):
		e.Server.Stop() // trigger a shutdown
		e.Close()
		return nil, fmt.Errorf("server took too long to start")
	}
	return e, nil
}

// applyDeltaSnapshot applies thw events from time sorted list of delta snapshot to etcd sequentially
func (r *Restorer) applyDeltaSnapshots(client *clientv3.Client, snapList snapstore.SnapList) error {
	for _, snap := range snapList {
		if err := r.applyDeltaSnapshot(client, *snap); err != nil {
			return err
		}
	}
	return nil
}

// applyDeltaSnapshot applies thw events from delta snapshot to etcd
func (r *Restorer) applyDeltaSnapshot(client *clientv3.Client, snap snapstore.Snapshot) error {
	events, err := getEventsFromDeltaSnapshot(r.store, snap)
	if err != nil {
		return fmt.Errorf("failed to read events from delta snapshot %s : %v", snap.SnapName, err)
	}
	return applyEventsToEtcd(client, events)
}

// getEventsFromDeltaSnapshot decodes the events from snapshot file.
func getEventsFromDeltaSnapshot(store snapstore.SnapStore, snap snapstore.Snapshot) ([]event, error) {
	size, err := store.Size(snap)
	if err != nil {
		return nil, err
	}
	rc, err := store.Fetch(snap)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b := make([]byte, size)
	if _, err = rc.Read(b); err != nil && err != io.EOF {
		return nil, err
	}
	events := []event{}
	err = json.Unmarshal(b, &events)
	return events, err
}

// applyEventsToEtcd performss operations in events sequentially
func applyEventsToEtcd(client *clientv3.Client, events []event) error {
	var (
		lastRev int64
		ops     = []clientv3.Op{}
		ctx     = context.TODO()
	)
	for _, e := range events {
		ev := e.EtcdEvent
		nextRev := ev.Kv.ModRevision
		if lastRev != 0 && nextRev > lastRev {
			if _, err := client.Txn(ctx).Then(ops...).Commit(); err != nil {
				return err
			}
			ops = []clientv3.Op{}
		}
		lastRev = nextRev
		switch ev.Type {
		case mvccpb.PUT:
			ops = append(ops, clientv3.OpPut(string(ev.Kv.Key), string(ev.Kv.Value))) //, clientv3.WithLease(clientv3.LeaseID(ev.Kv.Lease))))

		case mvccpb.DELETE:
			ops = append(ops, clientv3.OpDelete(string(ev.Kv.Key)))
		default:
			return fmt.Errorf("Unexpected event type")
		}
	}
	_, err := client.Txn(ctx).Then(ops...).Commit()
	return err
}
