// Copyright 2019 Liquidata, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package commands

import (
	"context"
	"fmt"
	"github.com/liquidata-inc/dolt/go/cmd/dolt/errhand"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/diff"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/doltdb"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/row"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/rowconv"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/schema"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/schema/typeinfo"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/sqle"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/table/pipeline"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/table/untyped"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/table/untyped/fwt"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/table/untyped/nullprinter"
	"github.com/liquidata-inc/dolt/go/libraries/utils/argparser"
	"github.com/liquidata-inc/dolt/go/libraries/utils/iohelp"
	"github.com/liquidata-inc/dolt/go/store/types"
	"github.com/liquidata-inc/go-mysql-server/sql"
	"io"
	"strings"

	"github.com/liquidata-inc/dolt/go/libraries/utils/filesys"

	"github.com/liquidata-inc/dolt/go/cmd/dolt/cli"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/env"
)

const (
	fromDB = "from"
	toDB   = "to"
)

//var diffDocs = cli.CommandDocumentationContent{
var queryDiffDocs = cli.CommandDocumentationContent{
	ShortDesc: "",
	LongDesc: "",
	Synopsis: nil,
}

type QueryDiffCmd struct {
	VersionStr string
}

// Name is returns the name of the Dolt cli command. This is what is used on the command line to invoke the command
func (cmd QueryDiffCmd) Name() string {
	return "query_diff"
}

// Description returns a description of the command
func (cmd QueryDiffCmd) Description() string {
	return "Diffs the results of a query between two roots"
}

// RequiresRepo should return false if this interface is implemented, and the command does not have the requirement
// that it be run from within a data repository directory
func (cmd QueryDiffCmd) RequiresRepo() bool {
	return true
}

// CreateMarkdown creates a markdown file containing the helptext for the command at the given path
func (cmd QueryDiffCmd) CreateMarkdown(fs filesys.Filesys, path, commandStr string) error {
	return nil
}

func (cmd QueryDiffCmd) createArgParser() *argparser.ArgParser {
	ap := argparser.NewArgParser()
	return ap
}

// Version displays the version of the running dolt client
// Exec executes the command
func (cmd QueryDiffCmd) Exec(ctx context.Context, commandStr string, args []string, dEnv *env.DoltEnv) int {
	ap := cmd.createArgParser()
	help, usage := cli.HelpAndUsagePrinters(cli.GetCommandDocumentation(commandStr, queryDiffDocs, ap))
	apr := cli.ParseArgs(ap, args, help)

	from, to, leftover, err := getDiffRoots(ctx, dEnv, apr.Args())

	var verr errhand.VerboseError
	if err != nil {
		verr = errhand.BuildDError("error determining diff commits for args: %s", strings.Join(apr.Args(), " ")).AddCause(err).Build()
		return HandleVErrAndExitCode(verr, usage)
	}
	if len(leftover) < 1 {
		verr = errhand.BuildDError("too many arguments: %s", strings.Join(apr.Args(), " ")).Build()
	} else if len(leftover) > 1 {
		verr = errhand.BuildDError("too many arguments: %s", strings.Join(apr.Args(), " ")).Build()
	}
	if verr != nil {
		return HandleVErrAndExitCode(verr, usage)
	}

	verr = diffQuery(ctx, dEnv, from, to, leftover[0])

	return HandleVErrAndExitCode(verr, usage)
}

func getDiffRoots(ctx context.Context, dEnv *env.DoltEnv, args []string) (from, to *doltdb.RootValue, leftover []string, err error) {
	headRoot, err := dEnv.StagedRoot(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	//workingRoot, err := dEnv.WorkingRootWithDocs(ctx) // todo: uncomment
	workingRoot, err := dEnv.WorkingRoot(ctx)
	if err != nil {
		return nil, nil, nil, err
	}

	if len(args) == 0 {
		// `dolt diff`
		from = headRoot
		to = workingRoot
		return from, to, nil, nil
	}

	from, ok := maybeResolve(ctx, dEnv, args[0])

	if !ok {
		// `dolt diff ...tables`
		from = headRoot
		to = workingRoot
		leftover = args
		return from, to, leftover, nil
	}

	if len(args) == 1 {
		// `dolt diff from_commit`
		to = workingRoot
		return from, to, nil, nil
	}

	to, ok = maybeResolve(ctx, dEnv, args[1])

	if !ok {
		// `dolt diff from_commit ...tables`
		to = workingRoot
		leftover = args[1:]
		return from, to, leftover, nil
	}

	// `dolt diff from_commit to_commit ...tables`
	leftover = args[2:]
	return from, to, leftover, nil
}

func maybeResolve(ctx context.Context, dEnv *env.DoltEnv, spec string) (*doltdb.RootValue, bool) {
	cs, err := doltdb.NewCommitSpec(spec, dEnv.RepoState.CWBHeadRef().String())
	if err != nil {
		return nil, false
	}

	cm, err := dEnv.DoltDB.Resolve(ctx, cs)
	if err != nil {
		return nil, false
	}

	root, err := cm.GetRootValue()
	if err != nil {
		return nil, false
	}

	return root, true
}

func diffQuery(ctx context.Context, dEnv *env.DoltEnv, fromRoot, toRoot *doltdb.RootValue, query string) errhand.VerboseError {
	fromCtx, fromEng, err := makeSqlEngine(ctx, dEnv, fromRoot)
	if err != nil {
		return errhand.VerboseErrorFromError(err)
	}
	toCtx, toEng, err := makeSqlEngine(ctx, dEnv, toRoot)
	if err != nil {
		return errhand.VerboseErrorFromError(err)
	}

	sch, fromIter, err := processQuery(fromCtx, query, fromEng)
	if err != nil {
		return formatQueryError("cannot execute query at from root", err)
	}

	toSch, toIter, err := processQuery(toCtx, query, toEng)
	if err != nil {
		return formatQueryError("cannot execute query at to root", err)
	}

	if !sch.Equals(toSch) {
		return errhand.BuildDError("cannot diff query, result schemas are not equal").Build()
	}

	ordFromIter, ok := fromIter.(sql.OrderableRowIter)
	if !ok {
		return errorWithQueryPlan(ctx, dEnv, fromRoot, query)
	}
	ordToIter, ok := toIter.(sql.OrderableRowIter)
	if !ok {
		return errorWithQueryPlan(ctx, dEnv, toRoot, query)
	}

	rowCmp, err := ordFromIter.RowCompareFunc(sch)
	if err != nil {
		return errorWithQueryPlan(ctx, dEnv, fromRoot, query)
	}

	doltSch := doltSchFromSqlSchema(sch)

	joiner, err := rowconv.NewJoiner(
		[]rowconv.NamedSchema{
			{Name: diff.From, Sch: doltSch},
			{Name: diff.To, Sch: doltSch},
		},
		map[string]rowconv.ColNamingFunc{diff.To: toNamer, diff.From: fromNamer},
	)
	if err != nil {
		return errhand.VerboseErrorFromError(err)
	}

	qd := &queryDiffer{
		sqlCtx:   fromCtx,
		fromIter: ordFromIter,
		toIter:   ordToIter,
		rowCmp:   rowCmp,
		sch:      sch,
		joiner:   joiner,
	}

	p, err := buildQueryDiffPipeline(qd, doltSch)

	if err != nil {
		return errhand.BuildDError("error building diff pipeline").AddCause(err).Build()
	}

	p.Start()

	return errhand.VerboseErrorFromError(p.Wait())
}

const db = "db"
func makeSqlEngine(ctx context.Context, dEnv *env.DoltEnv, root *doltdb.RootValue) (*sql.Context, *sqlEngine, error) {
	mrEnv := env.DoltEnvAsMultiEnv(dEnv)
	roots := map[string]*doltdb.RootValue{db: root}
	dbs := []sqle.Database{newDatabase(db, dEnv)}

	sqlCtx := sql.NewContext(ctx,
		sql.WithSession(sqle.DefaultDoltSession()),
		sql.WithIndexRegistry(sql.NewIndexRegistry()),
		sql.WithViewRegistry(sql.NewViewRegistry()))
	sqlCtx.SetCurrentDatabase(db)

	eng, err := newSqlEngine(sqlCtx, mrEnv, roots, formatTabular, dbs...)
	if err != nil {
		return nil, nil, err
	}

	return sqlCtx, eng, nil
}

func doltSchFromSqlSchema(sch sql.Schema) schema.Schema {
	dSch, _ := sqle.SqlSchemaToDoltResultSchema(sch)
	// make the first col the PK
	pk := false
	newCC, _ := schema.MapColCollection(dSch.GetAllCols(), func(col schema.Column) (column schema.Column, err error) {
		if !pk {
			col.IsPartOfPK = true
			pk = true
		}
		return col, nil
	})
	return schema.SchemaFromCols(newCC)
}

func errorWithQueryPlan(ctx context.Context, dEnv *env.DoltEnv, root *doltdb.RootValue, query string) errhand.VerboseError {
	sqlCtx, eng, err := makeSqlEngine(ctx, dEnv, root)
	if err != nil {
		return errhand.BuildDError("Cannot diff query, query is not ordered. Error describing query plan").AddCause(err).Build()
	}

	query = fmt.Sprintf("describe %s", query)
	_, iter, err := processQuery(sqlCtx, query, eng)
	if err != nil {
		return errhand.BuildDError("Cannot diff query, query is not ordered. Error describing query plan").AddCause(err).Build()
	}

	var qp strings.Builder
	for {
		r, err := iter.Next()
		if err == io.EOF {
			break
		} else if err != nil {
				return errhand.BuildDError("Cannot diff query, query is not ordered. Error describing query plan").AddCause(err).Build()
		}
		sv, _ := typeinfo.StringDefaultType.ConvertValueToNomsValue(r[0])
		qp.WriteString(fmt.Sprintf("%s\n",string(sv.(types.String))))
	}

	return errhand.BuildDError("Cannot diff query, query is not ordered. Add ORDER BY statement.\nquery plan:\n%s", qp.String()).Build()
}

type queryDiffer struct {
	sqlCtx   *sql.Context
	fromIter sql.RowIter
	toIter   sql.RowIter
	fromRow  sql.Row
	toRow    sql.Row
	rowCmp	 sql.RowCompareFunc
	sch      sql.Schema
	joiner   *rowconv.Joiner
}

func (qd *queryDiffer) nextDiff() (sql.Row, sql.Row, error) {
	fromEOF := false
	toEOF := false
	var err error
	for {
		if qd.fromRow == nil {
			qd.fromRow, err = qd.fromIter.Next()
			if err == io.EOF {
				fromEOF = true
			} else if err != nil {
				return nil, nil, err
			}
		}
		if qd.toRow == nil {
			qd.toRow, err = qd.toIter.Next()
			if err == io.EOF {
				toEOF = true
			} else if err != nil {
				return nil, nil, err
			}
		}
		if fromEOF && toEOF {
			return nil, nil, io.EOF
		}
		if fromEOF || toEOF {
			fromRow := qd.fromRow
			qd.fromRow = nil
			toRow := qd.toRow
			qd.toRow = nil
			return fromRow, toRow, nil
		}
		cmp, err := qd.rowCmp(qd.sqlCtx, qd.fromRow, qd.toRow)
		if err != nil {
			return nil, nil, err
		}
		switch cmp {
		case -1:
			fromRow := qd.fromRow
			qd.fromRow = nil
			return fromRow, nil, nil
		case 1:
			toRow := qd.toRow
			qd.toRow = nil
			return nil, toRow, nil
		case 0:
			eq, err := qd.fromRow.Equals(qd.toRow, qd.sch)
			if err != nil {
				return nil, nil, err
			}
			if eq {
				qd.fromRow = nil
				qd.toRow = nil
				continue
			} else {
				// todo: we don't have any way to detect updates at this point
				// if rows are ordered equally, but not equal in value, consider it a drop/add
				fromRow := qd.fromRow
				qd.fromRow = nil
				return fromRow, nil, nil
			}
		default:
			panic(fmt.Sprintf("rowCmp() returned incorrect value in queryDiffer: %d", cmp))
		}
	}
}

func (qd *queryDiffer) NextDiff() (row.Row, pipeline.ImmutableProperties, error) {
	fromRow, toRow, err := qd.nextDiff()
	if err != nil {
		return nil, pipeline.ImmutableProperties{}, err
	}

	rows := make(map[string]row.Row)
	if fromRow != nil {
		sch := qd.joiner.SchemaForName(diff.From)
		oldRow, err := sqle.SqlRowToDoltRow(types.Format_Default, fromRow, sch)
		if err != nil {
			return nil, pipeline.ImmutableProperties{}, err
		}
		rows[diff.From] = oldRow
	}

	if toRow != nil {
		sch := qd.joiner.SchemaForName(diff.To)
		newRow, err := sqle.SqlRowToDoltRow(types.Format_Default, toRow, sch)
		if err != nil {
			return nil, pipeline.ImmutableProperties{}, err
		}
		rows[diff.To] = newRow
	}

	joinedRow, err := qd.joiner.Join(rows)
	if err != nil {
		return nil, pipeline.ImmutableProperties{}, err
	}

	return joinedRow, pipeline.ImmutableProperties{}, nil
}

// todo: this logic was adapted from commands/diff.go, it could be simplified
func buildQueryDiffPipeline(qd *queryDiffer, doltSch schema.Schema) (*pipeline.Pipeline, error) {

	unionSch, ds, verr := createSplitter(doltSch, doltSch, qd.joiner, &diffArgs{diffOutput:TabularDiffOutput})
	if verr != nil {
		return nil, verr
	}

	transforms := pipeline.NewTransformCollection()
	nullPrinter := nullprinter.NewNullPrinter(unionSch)
	fwtTr := fwt.NewAutoSizingFWTTransformer(unionSch, fwt.HashFillWhenTooLong, 1000)
	transforms.AppendTransforms(
		pipeline.NewNamedTransform("split_diffs", ds.SplitDiffIntoOldAndNew),
		pipeline.NewNamedTransform(nullprinter.NullPrintingStage, nullPrinter.ProcessRow),
		pipeline.NamedTransform{Name: fwtStageName, Func: fwtTr.TransformToFWT},
	)

	badRowCB := func(trf *pipeline.TransformRowFailure) (quit bool) {
		verr := errhand.BuildDError("Failed transforming row").AddDetails(trf.TransformName).AddDetails(trf.Details).Build()
		cli.PrintErrln(verr.Error())
		return true
	}

	sink, err := diff.NewColorDiffSink(iohelp.NopWrCloser(cli.CliOut), doltSch, 1)
	if err != nil {
		return nil, err
	}

	sinkProcFunc := pipeline.ProcFuncForSinkFunc(sink.ProcRowWithProps)
	p := pipeline.NewAsyncPipeline(pipeline.ProcFuncForSourceFunc(qd.NextDiff), sinkProcFunc, transforms, badRowCB)

	p.RunAfter(func() {
		err := sink.Close()
		if err != nil {
			cli.PrintErrln(err)
		}
	})

	names := make(map[uint64]string, doltSch.GetAllCols().Size())
	_ = doltSch.GetAllCols().Iter(func(tag uint64, col schema.Column) (stop bool, err error) {
		names[tag] = col.Name
		return false, nil
	})
	schRow, err := untyped.NewRowFromTaggedStrings(types.Format_Default, unionSch, names)
	if err != nil {
		return nil, err
	}
	p.InjectRow(fwtStageName, schRow)

	return p, nil
}