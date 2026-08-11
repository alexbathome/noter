// Package store persists tasks in a local bbolt database.
package store

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Zones are the four columns of the board, in display order.
var Zones = []string{"todo", "doing", "blocked", "done"}

// ValidZone reports whether name is one of Zones.
func ValidZone(name string) bool { return slices.Contains(Zones, name) }

// Task is a single note. Content is free-form markdown; the first line doubles
// as the title shown on the board.
type Task struct {
	ID      string    `json:"id"`
	Zone    string    `json:"zone"`
	Pos     int       `json:"pos"`
	Content string    `json:"content"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Title is the first line of the content, used for the board display.
func (t Task) Title() string {
	line, _, _ := strings.Cut(t.Content, "\n")
	return strings.TrimSpace(line)
}

var bucket = []byte("tasks")

// Store is a bbolt-backed collection of tasks.
type Store struct{ db *bolt.DB }

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucket)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// List returns every task, ordered by zone then position.
func (s *Store) List() ([]Task, error) {
	var tasks []Task
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(_, v []byte) error {
			var t Task
			if err := json.Unmarshal(v, &t); err != nil {
				return err
			}
			tasks = append(tasks, t)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Zone != tasks[j].Zone {
			return tasks[i].Zone < tasks[j].Zone
		}
		if tasks[i].Pos != tasks[j].Pos {
			return tasks[i].Pos < tasks[j].Pos
		}
		return tasks[i].ID < tasks[j].ID
	})
	return tasks, nil
}

// ByZone groups every task into its zone, each slice ordered by position.
func (s *Store) ByZone() (map[string][]Task, error) {
	tasks, err := s.List()
	if err != nil {
		return nil, err
	}
	grouped := make(map[string][]Task, len(Zones))
	for _, z := range Zones {
		grouped[z] = []Task{}
	}
	for _, t := range tasks {
		if ValidZone(t.Zone) {
			grouped[t.Zone] = append(grouped[t.Zone], t)
		}
	}
	return grouped, nil
}

// Get returns a single task by ID.
func (s *Store) Get(id string) (Task, error) {
	var t Task
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucket).Get([]byte(id))
		if v == nil {
			return fmt.Errorf("task %q not found", id)
		}
		return json.Unmarshal(v, &t)
	})
	return t, err
}

// Create appends an empty task to the bottom of zone.
func (s *Store) Create(zone string) (Task, error) {
	if !ValidZone(zone) {
		return Task{}, fmt.Errorf("unknown zone %q", zone)
	}
	now := time.Now().UTC()
	t := Task{
		// Time-prefixed so IDs sort chronologically and stay unique per task.
		ID:      fmt.Sprintf("%d-%04d", now.UnixNano(), now.Nanosecond()%10000),
		Zone:    zone,
		Created: now,
		Updated: now,
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		t.Pos = nextPos(b, zone)
		return put(b, t)
	})
	return t, err
}

// SetContent replaces a task's body.
func (s *Store) SetContent(id, content string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		t, err := get(b, id)
		if err != nil {
			return err
		}
		t.Content = content
		t.Updated = time.Now().UTC()
		return put(b, t)
	})
}

// Move places a task at index within zone, shifting its new neighbours down.
// It handles both cross-zone moves and reordering inside one zone.
func (s *Store) Move(id, zone string, index int) error {
	if !ValidZone(zone) {
		return fmt.Errorf("unknown zone %q", zone)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		moved, err := get(b, id)
		if err != nil {
			return err
		}

		peers, err := zoneTasks(b, zone, id)
		if err != nil {
			return err
		}
		if index < 0 || index > len(peers) {
			index = len(peers)
		}
		peers = append(peers, Task{})
		copy(peers[index+1:], peers[index:])
		moved.Zone = zone
		moved.Updated = time.Now().UTC()
		peers[index] = moved

		for i, t := range peers {
			if t.Pos == i && t.ID != id {
				continue // already correctly positioned
			}
			t.Pos = i
			if err := put(b, t); err != nil {
				return err
			}
		}
		return nil
	})
}

// Delete removes a task.
func (s *Store) Delete(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Delete([]byte(id))
	})
}

func get(b *bolt.Bucket, id string) (Task, error) {
	var t Task
	v := b.Get([]byte(id))
	if v == nil {
		return t, fmt.Errorf("task %q not found", id)
	}
	return t, json.Unmarshal(v, &t)
}

func put(b *bolt.Bucket, t Task) error {
	v, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return b.Put([]byte(t.ID), v)
}

// zoneTasks returns the tasks in zone ordered by position, skipping exclude.
func zoneTasks(b *bolt.Bucket, zone, exclude string) ([]Task, error) {
	var tasks []Task
	err := b.ForEach(func(_, v []byte) error {
		var t Task
		if err := json.Unmarshal(v, &t); err != nil {
			return err
		}
		if t.Zone == zone && t.ID != exclude {
			tasks = append(tasks, t)
		}
		return nil
	})
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Pos < tasks[j].Pos })
	return tasks, err
}

func nextPos(b *bolt.Bucket, zone string) int {
	peers, err := zoneTasks(b, zone, "")
	if err != nil || len(peers) == 0 {
		return 0
	}
	return peers[len(peers)-1].Pos + 1
}
