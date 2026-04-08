package main

import (
	"testing"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateRoom(t *testing.T) {
	db := testDB(t)
	err := db.CreateRoom("weekly-sync", "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	room, err := db.GetRoom("weekly-sync")
	if err != nil {
		t.Fatalf("get room: %v", err)
	}
	if room.Slug != "weekly-sync" {
		t.Errorf("slug = %q, want %q", room.Slug, "weekly-sync")
	}
	if room.Active {
		t.Error("new room should not be active")
	}
}

func TestCreateRoomDuplicateSlug(t *testing.T) {
	db := testDB(t)
	err := db.CreateRoom("standup", "$2a$10$fakehash")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	err = db.CreateRoom("standup", "$2a$10$otherhash")
	if err == nil {
		t.Fatal("expected error for duplicate slug")
	}
}

func TestGetRoomNotFound(t *testing.T) {
	db := testDB(t)
	_, err := db.GetRoom("nonexistent")
	if err == nil {
		t.Fatal("expected error for missing room")
	}
}

func TestListRooms(t *testing.T) {
	db := testDB(t)
	db.CreateRoom("room-a", "$2a$10$hash1")
	db.CreateRoom("room-b", "$2a$10$hash2")
	rooms, err := db.ListRooms()
	if err != nil {
		t.Fatalf("list rooms: %v", err)
	}
	if len(rooms) != 2 {
		t.Fatalf("got %d rooms, want 2", len(rooms))
	}
}

func TestDeleteRoom(t *testing.T) {
	db := testDB(t)
	db.CreateRoom("to-delete", "$2a$10$hash")
	err := db.DeleteRoom("to-delete")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = db.GetRoom("to-delete")
	if err == nil {
		t.Fatal("room should be deleted")
	}
}

func TestSetRoomActive(t *testing.T) {
	db := testDB(t)
	db.CreateRoom("meeting", "$2a$10$hash")
	err := db.SetRoomActive("meeting", true)
	if err != nil {
		t.Fatalf("set active: %v", err)
	}
	room, _ := db.GetRoom("meeting")
	if !room.Active {
		t.Error("room should be active")
	}
	db.SetRoomActive("meeting", false)
	room, _ = db.GetRoom("meeting")
	if room.Active {
		t.Error("room should be inactive")
	}
}
