package targets

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/orzlee/displayKvm/desktop/app/presentation"
	"github.com/orzlee/displayKvm/desktop/i18n"
	"github.com/orzlee/displayKvm/internal/model"
)

type presenceView struct {
	Label      string
	Detail     string
	DetailKey  string
	Tone       string
	Diagnostic *presentation.Diagnostic
}

type itemView struct {
	Kind     string
	ID       string
	Name     string
	Caption  string
	Count    string
	Selected bool
}

type schemeView struct {
	ID       string
	Label    string
	LabelKey string
	Values   string
	Selected bool
}

type displayView struct {
	ID          string
	Name        string
	Member      bool
	Mapped      bool
	SchemeID    string
	Presence    presenceView
	Schemes     []schemeView
	OtherGroups string
}

type editorView struct {
	Kind     string
	ID       string
	Name     string
	Title    string
	Caption  string
	Displays []displayView
}

type saveView struct {
	Saved    string
	Applied  string
	Detail   string
	CommitID string
	CanQuery bool
	Locked   bool
}

// 页面只投影固定管理JSON，不执行计时或恢复。输入读值必须用指针，
// 因为0是有效读值，不能与未取得读值的nil合并为同一种“未知”。
type inputPreviewStatus struct {
	State           string                `json:"state"`
	Phase           string                `json:"phase"`
	TargetID        string                `json:"target_id"`
	ActiveDisplayID string                `json:"active_display_id,omitempty"`
	StartedAt       time.Time             `json:"started_at"`
	FinishedAt      *time.Time            `json:"finished_at,omitempty"`
	HoldUntil       *time.Time            `json:"hold_until,omitempty"`
	Displays        []inputPreviewDisplay `json:"displays"`
}

type inputPreviewDisplay struct {
	DisplayID     string  `json:"display_id"`
	OriginalInput *uint16 `json:"original_input,omitempty"`
	TargetInput   uint16  `json:"target_input"`
	ObservedInput *uint16 `json:"observed_input,omitempty"`
	SwitchStatus  string  `json:"switch_status"`
	RestoreStatus string  `json:"restore_status"`
	Detail        string  `json:"detail,omitempty"`
}

func (s inputPreviewStatus) active() bool { return s.State == "queued" || s.State == "running" }

func (s inputPreviewStatus) valid() bool {
	if s.State != "queued" && s.State != "running" && s.State != "completed" && s.State != "failed" && s.State != "cancelled" {
		return false
	}
	if s.Phase != "queued" && s.Phase != "pre_read" && s.Phase != "switching" && s.Phase != "holding" && s.Phase != "restoring" && s.Phase != "finished" {
		return false
	}
	if !model.ValidID(s.TargetID) {
		return false
	}
	for _, row := range s.Displays {
		if !model.ValidID(row.DisplayID) || row.SwitchStatus != "pending" && row.SwitchStatus != "skipped" && row.SwitchStatus != "verified" && row.SwitchStatus != "unknown" && row.SwitchStatus != "failed" {
			return false
		}
		if row.RestoreStatus != "pending" && row.RestoreStatus != "not_needed" && row.RestoreStatus != "restored" && row.RestoreStatus != "unknown" && row.RestoreStatus != "failed" && row.RestoreStatus != "conflict" {
			return false
		}
	}
	return true
}

type previewDisplayView struct {
	ID, Name, Original, Target, Observed, SwitchKey, RestoreKey, Detail string
	OriginalKnown, ObservedKnown                                        bool
}

type previewView struct {
	TargetID, SessionID, Hint, StateKey, PhaseKey, HoldUntil, ActiveDisplayID string
	CanStart, Active, Unknown                                                 bool
	Error                                                                     *model.Error
	Rows                                                                      []previewDisplayView
}

type pageView struct {
	Endpoint        string
	ResourcePath    string
	Theme           string
	Locale          string
	Revision        uint64
	Dirty           bool
	Ready           bool
	CanReload       bool
	Groups          []itemView
	Targets         []itemView
	Editor          *editorView
	Error           *model.Error
	ErrorMessageKey string
	Notice          string
	Save            saveView
	Preview         previewView
}

func (h *handler) previewView() previewView {
	view := previewView{SessionID: h.previewID, Unknown: h.previewUnknown, Error: h.previewError, Hint: "targets.preview.select_target"}
	if h.draft != nil && h.draft.Selected.Kind == "target" {
		view.TargetID = h.draft.Selected.ID
		view.Hint = "targets.preview.save_first"
		saved := h.base
		dirty := h.draft.Dirty
		if h.commit != nil && h.commit.Saved != nil && *h.commit.Saved && h.commit.Effective != nil && *h.commit.Effective {
			saved, dirty = h.draft.Config, false
		}
		for _, target := range saved.Targets {
			if string(target.ID) == view.TargetID && len(target.Inputs) > 0 && !dirty {
				view.CanStart, view.Hint = true, "targets.preview.saved_only"
			}
		}
	}
	if h.preview != nil {
		view.Active, view.StateKey, view.PhaseKey = h.preview.active(), "targets.preview.state."+h.preview.State, "targets.preview.phase."+h.preview.Phase
		view.ActiveDisplayID = h.preview.ActiveDisplayID
		if h.preview.HoldUntil != nil && h.preview.Phase == "holding" && !h.previewUnknown {
			view.HoldUntil = h.preview.HoldUntil.Format(time.RFC3339Nano)
		}
		for _, row := range h.preview.Displays {
			projected := previewDisplayView{ID: row.DisplayID, Name: row.DisplayID, Target: strconv.Itoa(int(row.TargetInput)), SwitchKey: "targets.preview.switch." + row.SwitchStatus, RestoreKey: "targets.preview.restore." + row.RestoreStatus, Detail: row.Detail}
			if row.OriginalInput != nil {
				projected.OriginalKnown, projected.Original = true, strconv.Itoa(int(*row.OriginalInput))
			}
			if row.ObservedInput != nil {
				projected.ObservedKnown, projected.Observed = true, strconv.Itoa(int(*row.ObservedInput))
			}
			for _, display := range h.base.Displays {
				if string(display.ID) == row.DisplayID {
					projected.Name = displayName(display.Name, row.DisplayID)
				}
			}
			view.Rows = append(view.Rows, projected)
		}
	}
	if view.Unknown || h.previewPending {
		view.StateKey, view.Hint = "targets.preview.state.unknown", "targets.preview.unknown_hint"
	}
	view.CanStart = view.CanStart && !view.Active && !view.Unknown && !h.previewPending
	return view
}

func (d *draft) present(presence map[model.DisplayID]presenceView, catalog i18n.Catalog) pageView {
	view := pageView{Dirty: d.Dirty, Ready: true}
	for _, group := range d.Config.Groups {
		view.Groups = append(view.Groups, itemView{
			Kind: "group", ID: string(group.ID), Name: displayName(group.Name, string(group.ID)),
			Caption: "targets.group_count", Count: strconv.Itoa(len(group.DisplayIDs)),
			Selected: d.Selected == (selection{Kind: "group", ID: string(group.ID)}),
		})
	}
	for _, target := range d.Config.Targets {
		view.Targets = append(view.Targets, itemView{
			Kind: "target", ID: string(target.ID), Name: displayName(target.Name, string(target.ID)),
			Caption: "targets.mapped_count", Count: strconv.Itoa(len(target.Inputs)),
			Selected: d.Selected == (selection{Kind: "target", ID: string(target.ID)}),
		})
	}
	var group *model.Group
	var target *model.Target
	for i := range d.Config.Groups {
		if d.Selected == (selection{Kind: "group", ID: string(d.Config.Groups[i].ID)}) {
			group = &d.Config.Groups[i]
		}
	}
	for i := range d.Config.Targets {
		if d.Selected == (selection{Kind: "target", ID: string(d.Config.Targets[i].ID)}) {
			target = &d.Config.Targets[i]
		}
	}
	if group == nil && target == nil {
		return view
	}
	editor := &editorView{Kind: d.Selected.Kind, ID: d.Selected.ID}
	if group != nil {
		editor.Name, editor.Title = group.Name, "targets.edit_group"
		editor.Caption = "targets.group_description"
	} else {
		editor.Name, editor.Title = target.Name, "targets.edit_target"
		editor.Caption = "targets.target_description"
	}
	for _, display := range d.Config.Displays {
		row := displayView{ID: string(display.ID), Name: displayName(display.Name, string(display.ID)), Presence: presence[display.ID]}
		if row.Presence.Label == "" {
			row.Presence = presenceView{Label: "targets.presence.unread", DetailKey: "targets.presence.unread_detail", Tone: "notice"}
		}
		if group != nil {
			for _, id := range group.DisplayIDs {
				row.Member = row.Member || id == display.ID
			}
		}
		if target != nil {
			for _, input := range target.Inputs {
				if input.DisplayID == display.ID {
					row.Mapped, row.SchemeID = true, string(input.SchemeID)
				}
			}
			selectedFound := false
			for _, scheme := range d.Config.InputSchemes {
				selectedFound = selectedFound || string(scheme.ID) == row.SchemeID
				key := "targets.scheme.existing"
				values := map[string]string{"id": string(scheme.ID), "value": strconv.Itoa(int(scheme.Value))}
				label := catalog.Text(key, values)
				// 校准端口名只在目标、显示器及完整方案都匹配时使用；历史依据
				// 可能来自旧映射，不能把旧端口名套到新的协议值上误导保存。
				for _, calibrated := range d.Config.Calibrations {
					if calibrated.DisplayID == display.ID && calibrated.TargetID == target.ID && calibrated.Scheme == scheme {
						label = calibrated.PortName + " · " + string(scheme.ID)
						key = ""
					}
				}
				encoded, _ := json.Marshal(values)
				row.Schemes = append(row.Schemes, schemeView{ID: string(scheme.ID), Label: label, LabelKey: key, Values: string(encoded), Selected: string(scheme.ID) == row.SchemeID})
			}
			if row.Mapped && !selectedFound {
				// 无效草稿也必须保留用户提交的值；否则浏览器会把未选中的列表默认为
				// 第一项，下次保存时静默抹掉错误映射，使核心校验原因难以纠正。
				values := map[string]string{"id": row.SchemeID}
				encoded, _ := json.Marshal(values)
				row.Schemes = append(row.Schemes, schemeView{ID: row.SchemeID, Label: catalog.Text("targets.scheme.invalid", values), LabelKey: "targets.scheme.invalid", Values: string(encoded), Selected: true})
			}
		}
		for _, other := range d.Config.Groups {
			if group != nil && other.ID == group.ID {
				continue
			}
			for _, id := range other.DisplayIDs {
				if id == display.ID {
					if row.OtherGroups != "" {
						row.OtherGroups += "、"
					}
					row.OtherGroups += displayName(other.Name, string(other.ID))
				}
			}
		}
		editor.Displays = append(editor.Displays, row)
	}
	view.Editor = editor
	return view
}

func displayName(name, id string) string {
	if name != "" {
		return name
	}
	return id
}
