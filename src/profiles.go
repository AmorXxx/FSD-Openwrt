package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

type profileState struct {
	Active string   `json:"active"`
	Names  []string `json:"names"`
}
type profileAction struct {
	Action string `json:"action"`
	Name   string `json:"name"`
	Source string `json:"source"`
	Active string `json:"active"`
}

func validProfile(name string) bool {
	if name == "" || len(name) > 80 || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != ' ' {
			return false
		}
	}
	return true
}
func (app *application) profileFile(name string) string {
	return filepath.Join(app.dir, "profiles", name+".yaml")
}
func (app *application) activeProfile() (string, error) {
	target, err := os.Readlink(filepath.Join(app.dir, "config.yaml"))
	if err != nil {
		return "", err
	}
	name := strings.TrimSuffix(filepath.Base(target), ".yaml")
	if !validProfile(name) || target != filepath.Join("profiles", name+".yaml") {
		return "", errors.New("无效的当前方案链接")
	}
	return name, nil
}
func (app *application) pointProfile(name string) error {
	tmp, err := os.CreateTemp(app.dir, ".profile-*")
	if err != nil {
		return err
	}
	path := tmp.Name()
	tmp.Close()
	os.Remove(path)
	defer os.Remove(path)
	if err = os.Symlink(filepath.Join("profiles", name+".yaml"), path); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(app.dir, "config.yaml"))
}
func (app *application) initProfiles() error {
	file := filepath.Join(app.dir, "config.yaml")
	info, err := os.Lstat(file)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		name, e := app.activeProfile()
		if e != nil {
			return e
		}
		target, e := os.Lstat(app.profileFile(name))
		if e != nil {
			return e
		}
		if !target.Mode().IsRegular() {
			return errors.New("方案必须为普通 YAML 文件")
		}
		return nil
	}
	if err = os.MkdirAll(filepath.Join(app.dir, "profiles"), 0700); err != nil {
		return err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	name := "默认方案"
	for n := 1; ; n++ {
		if _, e := os.Lstat(app.profileFile(name)); errors.Is(e, os.ErrNotExist) {
			break
		} else if e != nil {
			return e
		}
		name = fmt.Sprintf("默认方案 %d", n)
	}
	if err = writePrivate(app.profileFile(name), data); err != nil {
		return err
	}
	return app.pointProfile(name)
}
func (app *application) profilesState() (profileState, error) {
	active, err := app.activeProfile()
	if err != nil {
		return profileState{}, err
	}
	entries, err := os.ReadDir(filepath.Join(app.dir, "profiles"))
	if err != nil {
		return profileState{}, err
	}
	state := profileState{Active: active, Names: []string{}}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".yaml")
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".yaml") && validProfile(name) {
			state.Names = append(state.Names, name)
		}
	}
	return state, nil
}
func (app *application) changeProfile(input profileAction) error {
	state, err := app.profilesState()
	if err != nil {
		return err
	}
	if input.Active != state.Active {
		return errors.New("当前方案已改变，请刷新页面")
	}
	if !validProfile(input.Name) {
		return errors.New("名称仅支持文字、数字、空格、短横线和下划线，最长 80 字节")
	}
	path := app.profileFile(input.Name)
	regular := func(path string) error {
		info, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("方案必须为普通 YAML 文件")
		}
		return nil
	}
	switch input.Action {
	case "save", "copy", "rename":
		if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
			return errors.New("方案名称已存在或不可访问")
		}
		source := input.Source
		if input.Action == "save" {
			source = state.Active
		}
		if !validProfile(source) {
			return errors.New("无效的源方案")
		}
		old := app.profileFile(source)
		if err = regular(old); err != nil {
			return err
		}
		data, e := os.ReadFile(old)
		if e != nil {
			return e
		}
		cfg, e := decodeConfig(data)
		if e != nil {
			return e
		}
		data, e = yaml.Marshal(cfg)
		if e != nil {
			return e
		}
		if err = writePrivate(path, data); err != nil {
			return err
		}
		if input.Action == "rename" {
			if source == state.Active {
				if err = app.pointProfile(input.Name); err != nil {
					os.Remove(path)
					return err
				}
			}
			if err = os.Remove(old); err != nil {
				if source == state.Active {
					_ = app.pointProfile(source)
				}
				os.Remove(path)
				return err
			}
		}
	case "delete":
		if input.Name == state.Active {
			return errors.New("不能删除正在使用的方案")
		}
		if err = regular(path); err != nil {
			return err
		}
		return os.Remove(path)
	case "switch":
		if input.Name == state.Active {
			return nil
		}
		if err = regular(path); err != nil {
			return err
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		cfg, e := app.validateImport(data)
		if e != nil {
			return e
		}
		next, e := app.prepare(cfg)
		if e != nil {
			return e
		}
		if err = app.pointProfile(input.Name); err != nil {
			return err
		}
		if err = app.commitSettings(next, nil); err != nil {
			return errors.Join(err, app.pointProfile(state.Active))
		}
	default:
		return errors.New("未知方案操作")
	}
	return nil
}
func (app *application) profilesAPI(w http.ResponseWriter, r *http.Request) {
	app.update.Lock()
	defer app.update.Unlock()
	if err := app.initProfiles(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if r.Method == http.MethodPost {
		var input profileAction
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil || d.Decode(new(any)) != io.EOF {
			http.Error(w, "无效的方案请求", 400)
			return
		}
		if err := app.changeProfile(input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	} else if r.Method != http.MethodGet {
		http.Error(w, "use GET or POST", 405)
		return
	}
	state, err := app.profilesState()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	responseJSON(w, 200, state)
}
