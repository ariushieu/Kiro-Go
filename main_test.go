package main

import "testing"

func TestValidateAdminPath(t *testing.T) {
	valid := []string{"/panel", "panel", "/my-secret-panel/", "/ops/panel", "/Admin2"}
	for _, p := range valid {
		if err := validateAdminPath(p); err != nil {
			t.Errorf("validateAdminPath(%q): unexpected error %v", p, err)
		}
	}
	invalid := []string{"", "/", "//", "/v1", "/v1/panel", "/check", "/usage", "/assets", "/api", "/health", "/messages", "/V1"}
	for _, p := range invalid {
		if err := validateAdminPath(p); err == nil {
			t.Errorf("validateAdminPath(%q): expected error, got nil", p)
		}
	}
}
