// Package views holds the html/template assets for the httpapi handlers.
//
// Each parsed *template.Template is composed of layout.html plus exactly
// one page template. ExecuteTemplate renders the "layout" block; the page
// templates supply "title" and "content" blocks. layout.html itself never
// loads remote assets — Content-Security-Policy stays at default-src
// 'self'.
//
// SIN-62217 ships only the bare templates needed to prove the auth →
// tenancy → RLS stack end-to-end. HTMX integration lives in a follow-up
// PR alongside the static-asset route; until then layout is plain HTML.
package views

import (
	"embed"
	"html/template"
)

//go:embed *.html
var assets embed.FS

// Login renders GET /login and the re-rendered POST /login form on
// credential failure. Data shape: struct { Next, Error string }.
var Login = template.Must(template.ParseFS(assets, "layout.html", "login.html"))

// Hello renders GET /hello-tenant. Data shape: struct { TenantName,
// UserID string }.
var Hello = template.Must(template.ParseFS(assets, "layout.html", "hello.html"))
