import importlib.util
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('report', Path(__file__).with_name('anti-observation-report.py'))
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)


def observation(id, calls=0, complete=False, purpose='reply'):
    return dict(version=1, id=id, purpose=purpose, injected=True, sent=True,
                system_prompt=False, behavior='legacy', complete=complete,
                rounds=[dict(calls=calls, legal=calls, finish_reason='provider_19', other_output=False)])


def row(id, observations, anti=1):
    return dict(id=id, model='fixture', channel='playground', protocol='openai-chat', status=200,
                state='blocked', anti_truncation=anti, anti_observations=observations)


class TestAntiReport(unittest.TestCase):
    def test_selection_unknown_and_root_dedup(self):
        rows = [row('both', [observation('positive',1), observation('unknown')]),
                row('negative', [observation('negative',complete=True)]),
                row('summary', [observation('summary',1,True,'compaction')]),
                row('old', None), row('plain',None,0)]
        result = report.summarize(rows,True)
        self.assertEqual(result['roots']['http_200_blocked'],5)
        self.assertEqual(result['roots']['anti_observation_missing'],1)
        g = next(g for g in result['groups'] if g['purpose']=='reply')
        self.assertEqual([g[k] for k in ('T','H','N','U','C')],[3,1,1,1,1])
        self.assertEqual(g['observed_fraction_among_determined'],.5)
        self.assertEqual(g['complete_fraction'],1/3)
        self.assertEqual(sum(x['count'] for x in result['cross'] if x['dimensions'][0]=='200+blocked'),4)
        self.assertEqual(len(result['evidence']),4)

    def test_unknown_versions_unsent_and_exact_filters(self):
        from argparse import Namespace
        future=observation('future',1,True);future['version']=2
        unsent=observation('unsent');unsent.update(sent=False,end="not_sent")
        rows=[row('future',[future]),row('unsent',[unsent]),row('known-empty',[]),row('legacy',None)]
        result=report.summarize(rows,True)
        self.assertEqual(result['roots']['anti_no_lifecycle'],1)
        self.assertEqual(result['observation_field_fraction_among_anti_roots'],.75)
        self.assertEqual(result['groups'][0]['T'],0)
        self.assertEqual(result['groups'][0]['not_sent'],1)
        self.assertEqual(result['groups'][0]['unknown_version'],1)
        for r in rows:r['requested_model']='playground/anti-truncation/fixture'
        args=Namespace(anti='true',model='playground/anti-truncation/fixture',status=200)
        self.assertEqual(len(list(report.filter_rows(rows,args))),4)
        args.model='fixture'
        self.assertEqual(list(report.filter_rows(rows,args)),[])

    def test_statistics_classification_and_unevaluated(self):
        value=observation('raw-three',3,False)
        value['rounds']=[dict(calls=3,legal=1,empty=1,invalid=0,rejected=1)]
        real=observation('real-only',0,True);real['rounds'][0]['real_tools']=1
        none=observation('empty-response',0,True)
        fallback=observation('fallback',0,True);fallback['rounds'][0].update(ordinary_text=True,fallback=True)
        result=report.summarize([row('root',[value,real,none,fallback])])
        g=result['groups'][0]
        self.assertEqual([g[k] for k in ('T','H','N','U','C')],[4,1,3,0,3])
        self.assertEqual([g[k] for k in ('calls','legal','empty','rejected','unevaluated','pure_real_tools','no_output','fallback')],[3,1,1,1,1,1,1,1])
        self.assertEqual(result['roots']['http_200_blocked'],1)
        self.assertEqual(g['legal_fraction_among_observed_calls'],1/3)

    def test_root_send_union_and_presend_attempt_counts(self):
        before=observation("before");before.update(sent=False,end="not_sent");before["rounds"]=[dict(attempt_id="prep",sent=False)]
        reply=observation("reply",1,True);reply["rounds"][0].update(attempt_id="send",sent=True)
        summary=observation("summary",1,True,"compaction");summary["rounds"][0].update(attempt_id="summary-send",sent=True)
        early=observation("preparing");early.update(sent=False,end="preparing",rounds=[])
        rows=[row("mixed",[before,reply,summary]),row("before",[before]),row("early",[early]),row("old",None)]
        zero=row("explicit-zero",[]);zero["sent_attempt_count"]=0;rows.append(zero)
        rows[0]["sent_attempt_count"]=0 # later anti publication still wins with positive send evidence
        rows[2]["sent_attempt_count"]=0 # preparing is still unknown even with an explicit zero
        result=report.summarize(rows)
        self.assertEqual([result["roots"][k] for k in ("sent","not_sent","send_unknown")],[1,1,3])
        self.assertEqual(sum(g["physical_attempts"] for g in result["groups"]),4)
        self.assertEqual(sum(g["sent_attempts"] for g in result["groups"]),2)
        self.assertEqual(sum(g["T"] for g in result["groups"]),2)
        self.assertEqual(sum(g["send_unknown"] for g in result["groups"]),1)

    def test_other_output_not_no_output(self):
        value=observation("code",0,True);value["rounds"][0]["other_output"]=True
        g=report.summarize([row("root",[value])])["groups"][0]
        self.assertEqual([g[k] for k in ("other_output","no_output","pure_real_tools")],[1,0,0])

    def test_legacy_output_classification_remains_unknown(self):
        value=observation("legacy-no-output",0,True);del value["rounds"][0]["other_output"]
        g=report.summarize([row("root",[value])])["groups"][0]
        self.assertEqual([g[k] for k in ("no_output","pure_real_tools","output_classification_unknown")],[0,0,1])
        value.update(sent=False,end="preparing")
        sample=row("root",[value]);sample["sent_attempt_count"]=0
        self.assertEqual(report.root_send_state(sample,[value]),"send_unknown")
        value["end"]="not_sent"
        self.assertEqual(report.root_send_state(sample,[value]),"not_sent")
        value["sent"]=True
        self.assertEqual(report.root_send_state(sample,[value]),"sent")

    def test_zero_and_duplicate(self):
        result=report.summarize([])
        self.assertIsNone(result['blocked_rate_http200'])
        with self.assertRaises(ValueError):
            report.summarize([row('duplicate',[observation('same'),observation('same')])])
        with self.assertRaises(ValueError):
            report.summarize([row("same-root",None),row("same-root",None)])

    def test_readonly_legacy_and_new_columns(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'ledger.db'
            db=sqlite3.connect(path)
            db.execute('CREATE TABLE requests(id TEXT,time INTEGER,model TEXT,channel TEXT,protocol TEXT,status INTEGER,state TEXT)')
            db.execute("INSERT INTO requests VALUES ('old',1000,'fixture','playground','openai-chat',200,'blocked')")
            db.commit()
            before=path.read_bytes()
            self.assertIsNone(list(report.read_rows(path,0,2000))[0]['anti_observations'])
            self.assertEqual(path.read_bytes(),before)
            db.execute("CREATE TABLE request_metadata(id TEXT,anti_truncation INTEGER,base_model TEXT,anti_observations TEXT,requested_model TEXT)")
            db.execute("INSERT INTO request_metadata VALUES (?,?,?,?,?)", ("old",1,"fixture",json.dumps([observation("new",1,True)]),"fixture"))
            db.commit(); db.close()
            result=report.summarize(report.read_rows(path,0,2000))
            self.assertEqual(result['groups'][0]['H'],1)


if __name__ == '__main__':
    unittest.main()
