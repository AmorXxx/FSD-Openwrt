package main

import (
	"bytes"
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
	MonitorOnly bool     `json:"monitor_only"`
	Active      string   `json:"active"`
	Names       []string `json:"names"`
	Pending     bool     `json:"pending"`
}
type profileAction struct {
	Action   string        `json:"action"`
	Name     string        `json:"name"`
	Source   string        `json:"source"`
	Active   string        `json:"active"`
	YAML     string        `json:"yaml,omitempty"`
	Revision string        `json:"revision,omitempty"`
	Editor   *editorUpdate `json:"editor,omitempty"`
}

func validProfile(name string) bool {
	if name == "" || len(name) > 255 || strings.TrimSpace(name) != name || strings.HasPrefix(name, ".") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".yaml" && ext != ".yml" {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune(`/\`, r) {
			return false
		}
	}
	return true
}
func (app *application) profileFile(name string) string {
	return filepath.Join(app.dir, "profiles", name)
}
func (app *application) activeProfile() (string, error) {
	target, err := os.Readlink(filepath.Join(app.dir, "config.yaml"))
	if err != nil {
		return "", err
	}
	name := filepath.Base(target)
	if !validProfile(name) || target != filepath.Join("profiles", name) {
		return "", errors.New("无效的当前配置文件链接")
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
	if err = os.Symlink(filepath.Join("profiles", name), path); err != nil {
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
			return errors.New("配置文件必须为普通 YAML 文件")
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
	name := "config.yaml"
	for n := 1; ; n++ {
		if _, e := os.Lstat(app.profileFile(name)); errors.Is(e, os.ErrNotExist) {
			break
		} else if e != nil {
			return e
		}
		name = fmt.Sprintf("config-%d.yaml", n)
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
	state := profileState{MonitorOnly: app.current.Load().MonitorOnly, Active: active, Names: []string{}}
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && validProfile(name) {
			state.Names = append(state.Names, name)
		}
	}
	if _, disk, e := app.readProfileDocument(active); e == nil {
		a, _ := yaml.Marshal(disk)
		b, _ := yaml.Marshal(app.current.Load().config)
		state.Pending = !bytes.Equal(a, b)
	} else {
		state.Pending = true
	}
	return state, nil
}
func (app *application) changeProfile(input profileAction) error {
	state, err := app.profilesState()
	if err != nil {
		return err
	}
	if input.Active != state.Active {
		return errors.New("当前配置文件已改变，请刷新页面")
	}
	if !validProfile(input.Name) {
		return errors.New("请使用 .yaml 或 .yml 文件名，不能包含路径、控制字符或以点开头，最长 255 字节")
	}
	path := app.profileFile(input.Name)
	regular := func(path string) error {
		info, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("配置文件必须为普通 YAML 文件")
		}
		return nil
	}
	switch input.Action {
	case "apply":
		if input.Name != state.Active {
			return errors.New("请先切换到要编辑的 YAML 文件")
		}
		if len(input.YAML) > 128*1024 {
			return errors.New("YAML 文件上限为 128 KiB")
		}
		return app.replaceConfigLocked([]byte(input.YAML))
	case "import":
		if len(input.YAML) > 128*1024 {
			return errors.New("YAML 文件上限为 128 KiB")
		}
		if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
			return errors.New("同名 YAML 文件已存在，未覆盖；请重命名本地文件后导入")
		}
		if _, e := app.validateImport([]byte(input.YAML)); e != nil {
			return e
		}
		return writePrivate(path, []byte(input.YAML))

	case "save", "copy", "rename":
		if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
			return errors.New("配置文件名称已存在或不可访问")
		}
		source := input.Source
		if input.Action == "save" {
			source = state.Active
		}
		if !validProfile(source) {
			return errors.New("无效的源配置文件")
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
			return errors.New("不能删除正在使用的配置文件")
		}
		if err = regular(path); err != nil {
			return err
		}
		return os.Remove(path)
	case "switch":
		if input.Revision != "" {
			doc, _, e := app.readProfileDocument(input.Name)
			if e != nil {
				return e
			}
			if doc.Revision != input.Revision {
				return errors.New("文件已被修改，请重新读取后再应用")
			}
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
		return errors.New("未知配置文件操作")
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
	if r.Method == http.MethodGet && r.URL.Query().Has("name") {
		doc, _, err := app.readProfileDocument(r.URL.Query().Get("name"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		responseJSON(w, 200, doc)
		return
	}
	if r.Method == http.MethodPost {
		var input profileAction
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024))
		d.DisallowUnknownFields()
		if err := d.Decode(&input); err != nil || d.Decode(new(any)) != io.EOF {
			http.Error(w, "无效的配置文件请求", 400)
			return
		}
		if input.Action == "read" || input.Action == "write_yaml" || input.Action == "write_editor" {
			doc, err := app.editProfileDocument(input)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			responseJSON(w, 200, doc)
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
