package main

import (
	"errors"
	"strings"

	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
)

func cmdAIFind(inv *invocation) int {
	if inv.rawOutput {
		return inv.misuse("--raw is available for show and resource reads")
	}
	args, code, ok := parseAIArgs(inv, inv.data[1:], true)
	if !ok {
		return code
	}
	query := strings.TrimSpace(strings.Join(args.words, " "))
	if query == "" {
		return inv.misuse(`say what to look for, as in tt ai find "what to buy at the store"`)
	}
	if args.rerank && args.noRerank {
		return inv.misuse("choose --rerank or --no-ai-rerank, not both")
	}
	if code, ok := aiRefuseSecrets(inv, query, args.allowSecrets); !ok {
		return code
	}
	cfg, err := inv.config()
	if err != nil {
		return inv.fail(err)
	}
	if code, ok := aiKnownProfile(inv, cfg, args.request.Profile); !ok {
		return code
	}
	if inv.interrupted() {
		return exitInterrupted
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()

	rerank := app.AIRerankAuto
	switch {
	case args.rerank:
		rerank = app.AIRerankAlways
	case args.noRerank:
		rerank = app.AIRerankNever
	}
	now := aiNow()
	if err := app.PruneAIPreviews(inv.ctx, st, now); err != nil {
		return inv.fail(err)
	}
	finder := app.AIFinder{Store: st, Runner: newAIRunner(cfg.AI), Now: now, Context: cfg.AI.Context, AllowSecrets: args.allowSecrets}
	result, err := finder.Find(inv.ctx, query, rerank, args.request)
	if err != nil {
		if inv.interrupted() {
			return exitInterrupted
		}
		return aiFindFailure(inv, err, result.LeftOut)
	}
	if inv.jsonOutput {
		inv.resultData = result
		return exitOK
	}
	if err := inv.listTasksAt(st, result.Tasks, now); err != nil {
		return inv.fail(err)
	}
	var lines []string
	if len(result.Tasks) == 0 {
		lines = append(lines, "no cached task matches the search the model wrote")
		for _, word := range result.Filter.Keywords {
			lines = append(lines, cli.ReportLine("  keyword ", word, "", nil))
		}
	}
	lines = append(lines, aiLeftOutLines(result.LeftOut)...)
	cli.WriteLines(inv.stdout, lines)
	return exitOK
}

func aiFindFailure(inv *invocation, err error, leftOut app.AILeftOut) int {
	var invalid *app.AIInvalidError
	if !errors.As(err, &invalid) {
		return aiCallFailure(inv, err, leftOut)
	}
	return aiInvalid(inv, app.AIProposal{Issues: invalid.Issues, LeftOut: leftOut}, "the search the model wrote cannot be used")
}
