package main

import (
	"strconv"
	"strings"
)

// A standalone star is the catch-all; other stars occupy one path segment.
func validRoutePattern(pattern string) bool {
	for _, segment := range strings.Split(pattern, "/") {
		if strings.Contains(segment, "*") && segment != "*" {
			return false
		}
	}
	return true
}

func routeMatches(pattern, path string) bool {
	if pattern == "*" {
		return strings.HasPrefix(path, "/")
	}
	want, got := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i, segment := range want {
		if segment == "*" {
			if got[i] == "" {
				return false
			}
		} else if segment != got[i] {
			return false
		}
	}
	return true
}

// Exact paths win, then segment patterns, then catch-all. Ties keep YAML order.
func matchingRoute(routes []customRoute, path string) (customRoute, bool) {
	var selected customRoute
	best, found := 0, false
	for _, route := range routes {
		if !routeMatches(route.Path, path) {
			continue
		}
		stars := strings.Count(route.Path, "*")
		if route.Path == "*" {
			stars = 1000
		}
		if !found || stars < best {
			selected, best, found = route, stars, true
		}
	}
	return selected, found
}

// Exercise a real nonempty segment, avoiding exact/more-specific configured routes.
func diagnosticRoutePath(pattern string, routes []customRoute) string {
	if pattern == "*" {
		depth := 1
		for _, route := range routes {
			if n := strings.Count(route.Path, "/") + 1; n > depth {
				depth = n
			}
		}
		return strings.Repeat("/fsd-check", depth)
	}

	literals := make(map[string]bool)
	for _, route := range routes {
		for _, segment := range strings.Split(route.Path, "/") {
			literals[segment] = true
		}
	}
	for n := 0; ; n++ {
		token := "fsd-check-" + strconv.Itoa(n)
		if !literals[token] {
			return strings.ReplaceAll(pattern, "*", token)
		}
	}
}
