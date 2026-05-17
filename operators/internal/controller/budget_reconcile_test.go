/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

package controller

import "testing"

func TestAddDecimal(t *testing.T) {
	cases := []struct {
		a, b, want string
	}{
		{"0", "0", "0.000000"},
		{"1500.50", "10.25", "1510.750000"},
		{"0", "0.000123", "0.000123"},
		{"999999.999999", "0.000001", "1000000.000000"},
	}
	for _, c := range cases {
		got, err := addDecimal(c.a, c.b)
		if err != nil {
			t.Errorf("addDecimal(%q,%q) error: %v", c.a, c.b, err)
			continue
		}
		if got != c.want {
			t.Errorf("addDecimal(%q,%q) = %q; want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestPercentOfBudget(t *testing.T) {
	cases := []struct {
		spend, monthly string
		want           int32
	}{
		{"0", "1000", 0},
		{"500", "1000", 50},
		{"1000", "1000", 100},
		{"1200", "1000", 120},
		{"2500", "1000", 250},
		{"100", "0", 0},   // degenerate: 0 budget → 0%
		{"-5", "1000", 0}, // shouldn't happen but guard the sign
	}
	for _, c := range cases {
		got, err := percentOfBudget(c.spend, c.monthly)
		if err != nil {
			t.Errorf("percentOfBudget(%q,%q) error: %v", c.spend, c.monthly, err)
			continue
		}
		if got != c.want {
			t.Errorf("percentOfBudget(%q,%q) = %d; want %d", c.spend, c.monthly, got, c.want)
		}
	}
}

func TestShouldAlertAt(t *testing.T) {
	thresholds := []int32{50, 80, 100}
	cases := []struct {
		name             string
		lastPct, currPct int32
		want             int32
	}{
		{"no_change_below", 30, 40, 0},
		{"crossed_50_only", 30, 60, 50},
		{"crossed_50_and_80", 30, 90, 80},
		{"jumped_to_breach", 0, 130, 100},
		{"no_new_threshold", 85, 95, 0},
		{"empty_thresholds", 0, 200, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ths := thresholds
			if c.name == "empty_thresholds" {
				ths = nil
			}
			got := shouldAlertAt(ths, c.lastPct, c.currPct)
			if got != c.want {
				t.Errorf("shouldAlertAt(%v,%d,%d) = %d; want %d", ths, c.lastPct, c.currPct, got, c.want)
			}
		})
	}
}

func TestEscapeSQL(t *testing.T) {
	in := "tenant-acme"
	if got := escapeSQL(in); got != in {
		t.Errorf("escapeSQL(%q) = %q; want unchanged", in, got)
	}
	if got := escapeSQL("o'malley"); got != "o''malley" {
		t.Errorf("single quote not doubled: got %q", got)
	}
}
