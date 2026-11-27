package main

func withTestRoutes(cfg config, routes []customRoute) config {
	for i := range cfg.Sites {
		cfg.Sites[i].Routes = append([]customRoute{}, routes...)
	}
	return cfg
}

func editorSitesForTest(sites []siteConfig) *[]editorSiteUpdate {
	out := []editorSiteUpdate{}
	for _, s := range sites {
		r := []editorRouteUpdate{}
		for _, route := range s.Routes {
			mode := route.ResponseMode
			r = append(r, editorRouteUpdate{Path: route.Path, JSON: route.JSON, ResponseMode: &mode})
		}
		out = append(out, editorSiteUpdate{Domain: s.Domain, Certificate: s.Certificate, Key: s.Key, Routes: r})
	}
	return &out
}
func adaptEditorTest(input editorUpdate, sites []siteConfig) editorUpdate {
	if input.Sites == nil && input.Routes != nil {
		input.Sites = editorSitesForTest(sites)
		for i := range *input.Sites {
			(*input.Sites)[i].Routes = input.Routes
		}
	}
	return input
}
func (app *application) testSaveEditor(input editorUpdate) (editorSnapshot, error) {
	return app.saveEditor(adaptEditorTest(input, app.current.Load().Sites))
}
func (app *application) testEditProfileDocument(input profileAction) (profileDocument, error) {
	if input.Editor != nil {
		_, cfg, err := app.readProfileDocument(input.Name)
		if err != nil {
			return profileDocument{}, err
		}
		u := adaptEditorTest(*input.Editor, cfg.Sites)
		input.Editor = &u
	}
	return app.editProfileDocument(input)
}
