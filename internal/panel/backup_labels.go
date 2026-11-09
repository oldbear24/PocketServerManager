package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/backup"
	"github.com/oldbear24/PocketServerManager/internal/schema"
	"github.com/oldbear24/PocketServerManager/internal/stacks"
)

// A service's pocketcloud.backup label backs it up (#29). After every deploy
// that succeeds, the panel makes the stack's label jobs what the deployed
// revision's labels say: a job of the stack's server for each labelled
// service, named <stack>-<service>. What it backs up always comes from its
// label; its schedule, targets and retention only where the labels give
// them, so that what an admin chose for a job stays where its labels say
// nothing. A label that is gone leaves its job in place, disabled and marked,
// until someone removes it.

// syncBackupLabels makes the label jobs of a stack what its labels say, and
// returns what is wrong with them for the stack's page.
func (p *Panel) syncBackupLabels(stack *core.Record, labels []stacks.BackupLabel) []labelProblem {
	existing, err := p.App.FindRecordsByFilter(schema.BackupJobs, "stack={:stack} && origin={:label}", "", 0, 0,
		dbx.Params{"stack": stack.Id, "label": schema.JobLabel})
	if err != nil {
		p.Log.Error("failed to read the label backup jobs", "stack", stack.Id, "error", err)
		return nil
	}
	byService := map[string]*core.Record{}
	for _, job := range existing {
		byService[job.GetString("service")] = job
	}

	problems := []labelProblem{}
	labelled := map[string]bool{}
	for _, label := range labels {
		labelled[label.Service] = true
		// a label with a mistake holds its job as it is
		if label.Broken {
			continue
		}
		for _, problem := range p.applyBackupLabel(stack, label, byService[label.Service]) {
			problems = append(problems, labelProblem{Message: label.Service + ": " + problem})
		}
	}

	for service, job := range byService {
		if labelled[service] || job.GetBool("label_removed") {
			continue
		}
		err := p.update(context.Background(), job, func(fresh *core.Record) error {
			if fresh.GetString("origin") != schema.JobLabel || fresh.GetString("stack") != stack.Id || fresh.GetBool("label_removed") {
				return errUnchanged
			}
			fresh.Set("label_removed", true)
			fresh.Set("enabled", false)
			fresh.Set("next_run_at", "")
			return nil
		})
		if err != nil {
			p.Log.Error("failed to mark a label backup job", "job", job.Id, "error", err)
		}
	}
	return problems
}

// applyBackupLabel creates or changes the job of one service's label. It
// returns what is wrong with the label's targets.
func (p *Panel) applyBackupLabel(stack *core.Record, label stacks.BackupLabel, job *core.Record) []string {
	fields := map[string]any{
		"host": stack.GetString("host"), "stack": stack.Id, "service": label.Service,
		"origin": schema.JobLabel, "label_removed": false,
	}
	switch label.Kind {
	case stacks.BackupVolumes:
		fields["source_kind"] = schema.SourceStack
		fields["source"] = strings.Join(label.Volumes, ",")
		fields["engine"] = ""
	case stacks.BackupSQLite:
		fields["source_kind"] = schema.SourceDump
		fields["source"] = label.File
		fields["engine"] = schema.DumpSQLite
	default:
		fields["source_kind"] = schema.SourceDump
		fields["source"] = ""
		fields["engine"] = label.Kind
	}
	sets := []string{}
	if label.Schedule != "" {
		fields["schedule"] = label.Schedule
		sets = append(sets, labelSetsSchedule)
	}
	var problems []string
	if label.TargetsSet {
		var targets []string
		targets, problems = p.labelTargets(stack, label.Targets)
		fields["targets"] = targets
		sets = append(sets, labelSetsTargets)
	}
	if label.Retention != nil {
		fields["retention"] = *label.Retention
		sets = append(sets, labelSetsRetention)
	}
	fields["label_sets"] = sets

	settle := func(record *core.Record) {
		for name, value := range fields {
			record.Set(name, value)
		}
		problem := strings.Join(problems, " ")
		if len(record.GetStringSlice("targets")) == 0 {
			problem = strings.TrimSpace(problem + " " + noTarget)
		}
		record.Set("problem", cutText(problem, 1000))
		record.Set("next_run_at", nextRunOf(record, time.Now()))
	}

	if job == nil {
		collection, err := p.App.FindCachedCollectionByNameOrId(schema.BackupJobs)
		if err != nil {
			return []string{err.Error()}
		}
		job = core.NewRecord(collection)
		job.Set("name", p.freeJobName(stack.GetString("name")+"-"+label.Service))
		job.Set("enabled", true)
		job.Set("schedule", stacks.DefaultBackupSchedule)
		retention, _ := stacks.ParseRetention(stacks.DefaultBackupRetention)
		job.Set("retention", retention)
		job.Set("targets", []string{})
		settle(job)
		if err := p.App.Save(job); err != nil {
			return append(problems, "its backup job could not be saved: "+saveErrorText(err))
		}
		return problems
	}

	err := p.update(context.Background(), job, func(fresh *core.Record) error {
		if fresh.GetString("origin") != schema.JobLabel || fresh.GetString("stack") != stack.Id {
			return errUnchanged
		}
		before, _ := json.Marshal(fresh.FieldsData())
		// a label that is back runs again
		if fresh.GetBool("label_removed") {
			fresh.Set("enabled", true)
		}
		settle(fresh)
		after, _ := json.Marshal(fresh.FieldsData())
		if string(before) == string(after) {
			return errUnchanged
		}
		return nil
	})
	if err != nil {
		return append(problems, "its backup job could not be saved: "+saveErrorText(err))
	}
	return problems
}

// labelTargets finds the targets a label names. A name that is no target,
// or a local target of another server, is a problem.
func (p *Panel) labelTargets(stack *core.Record, names []string) ([]string, []string) {
	ids := []string{}
	problems := []string{}
	for _, name := range names {
		target, err := p.App.FindFirstRecordByData(schema.BackupTargets, "name", name)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s names %s, which is no backup target.", stacks.LabelBackupTarget, name))
		case target.GetString("kind") == backup.KindLocal && target.GetString("host") != stack.GetString("host"):
			problems = append(problems, localElsewhere(target))
		default:
			ids = append(ids, target.Id)
		}
	}
	return ids, problems
}

// freeJobName is a job name that no job has: the name, or the name with a
// number after it.
func (p *Panel) freeJobName(name string) string {
	base := name
	if utf8.RuneCountInString(base) > 95 {
		base = string([]rune(base)[:95])
	}
	candidate := base
	for i := 2; i < 1000; i++ {
		if _, err := p.App.FindFirstRecordByData(schema.BackupJobs, "name", candidate); err != nil {
			return candidate
		}
		candidate = base + "-" + strconv.Itoa(i)
	}
	return base + "-" + strings.ToLower(core.GenerateDefaultRandomId()[:6])
}

// markOrphanedLabelJobs marks the label jobs of stacks that are gone, as
// their labels are, and says of the other jobs of a removed stack that it is
// gone.
func (p *Panel) markOrphanedLabelJobs() {
	records, err := p.App.FindRecordsByFilter(schema.BackupJobs,
		"stack='' && ((origin={:label} && label_removed=false) || (origin!={:label} && (source_kind={:stack} || source_kind={:dump}) && problem=''))", "", 0, 0,
		dbx.Params{"label": schema.JobLabel, "stack": schema.SourceStack, "dump": schema.SourceDump})
	if err != nil {
		return
	}
	for _, record := range records {
		err := p.update(context.Background(), record, func(fresh *core.Record) error {
			if fresh.GetString("stack") != "" {
				return errUnchanged
			}
			if fresh.GetString("origin") == schema.JobLabel {
				if fresh.GetBool("label_removed") {
					return errUnchanged
				}
				fresh.Set("label_removed", true)
				fresh.Set("enabled", false)
				fresh.Set("next_run_at", "")
				return nil
			}
			if fresh.GetString("problem") != "" {
				return errUnchanged
			}
			fresh.Set("problem", "Its stack was removed.")
			return nil
		})
		if err != nil {
			p.Log.Error("failed to mark a backup job of a removed stack", "job", record.Id, "error", err)
		}
	}
}
