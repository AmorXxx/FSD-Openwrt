package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"os"
)

type profileDocument struct {
	Name     string         `json:"name"`
	YAML     string         `json:"yaml"`
	Revision string         `json:"revision"`
	Editor   editorSnapshot `json:"editor"`
}

func (app *application) readProfileDocument(name string) (profileDocument, config, error) {
	var doc profileDocument
	if !validProfile(name) {
		return doc, config{}, errors.New("无效的 YAML 文件名")
	}
	path := app.profileFile(name)
	info, err := os.Lstat(path)
	if err != nil {
		return doc, config{}, err
	}
	if !info.Mode().IsRegular() {
		return doc, config{}, errors.New("必须是普通 YAML 文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return doc, config{}, err
	}
	cfg, err := decodeConfig(data)
	if err != nil {
		return doc, cfg, err
	}
	public, err := yaml.Marshal(cfg.publicConfig())
	if err != nil {
		return doc, cfg, err
	}
	revision := fmt.Sprintf("%x:%s", sha256.Sum256(data), name)
	editor := snapshotForEditor(cfg)
	editor.Revision = revision
	return profileDocument{Name: name, YAML: string(public), Revision: revision, Editor: editor}, cfg, nil
}

// Caller holds app.update. Disk edits never replace the running settings.
func (app *application) editProfileDocument(input profileAction) (profileDocument, error) {
	doc, cfg, err := app.readProfileDocument(input.Name)
	if err != nil {
		return doc, err
	}
	if input.Action == "read" {
		return doc, nil
	}
	if input.Revision != doc.Revision {
		return doc, errors.New("文件已被修改，请重新读取后再保存")
	}
	switch input.Action {
	case "write_yaml":
		if len(input.YAML) > 128*1024 {
			return doc, errors.New("YAML 文件上限为 128 KiB")
		}
		next, e := app.validateImport([]byte(input.YAML))
		if e != nil {
			return doc, e
		}
		cfg = next
	case "write_editor":
		u := input.Editor
		if u == nil || (u.DNS == nil && u.DNSRules == nil) || u.Sites == nil {
			return doc, errors.New("缺少编辑内容")
		}
		if u.Revision != doc.Revision {
			return doc, errors.New("编辑内容已过期，请重新读取")
		}
		sites, e := u.updatedSites(cfg.Sites)
		if e != nil {
			return doc, e
		}
		cfg.DNS, cfg.Sites = u.DNS, sites
		if u.Scope != nil {
			cfg.Scope = *u.Scope
		}
		if u.DNSPassthrough != nil {
			cfg.DNSPassthrough = append([]string{}, (*u.DNSPassthrough)...)
		}
		if u.Blacklist != nil {
			if u.Blacklist.Block == nil || u.Blacklist.Allow == nil {
				return doc, errors.New("dns_blacklist 必须包含 block 和 allow 列表")
			}
			cfg.Blacklist = *u.Blacklist
		}
		if u.DNSRules != nil {
			cfg.setDNSRules(*u.DNSRules)
		}
		if e = cfg.validate(); e != nil {
			return doc, e
		}
		if u.Sites != nil {
			for _, site := range cfg.Sites {
				if site.Certificate != "" {
					if _, e = customCertificate(site); e != nil {
						return doc, e
					}
				}
			}
		}
	default:
		return doc, errors.New("未知文件编辑操作")
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return doc, err
	}
	if err = writePrivate(app.profileFile(input.Name), data); err != nil {
		return doc, err
	}
	doc, _, err = app.readProfileDocument(input.Name)
	return doc, err
}
