"""Checker regression only: no server process and no claim of live CPA coverage."""
import json
from pathlib import Path
import unittest

from check_evidence import read_error_evidence


class ReadErrorProjectionTest(unittest.TestCase):
    def test_approved_cpa_error_with_normal_downstream_eof(self):
        root=Path(__file__).resolve().parents[2]
        path=root/'coordination/diagnostics/20260924/DIAG-05-R2-read-synthetic.jsonl'
        records=[json.loads(line.removeprefix('@diag ')) for line in path.read_text(encoding='utf-8').splitlines()]
        attempts=[('approved-r2-export',r) for r in records if r['event']=='upstream.attempt_finished']
        self.assertTrue(attempts)
        for wire in ('data: {"error":{"message":"synthetic"}}\n\n','event: response.completed\ndata: {}\n\n'):
            response={'readError':None,'body':wire}
            self.assertEqual(read_error_evidence('cpa1',response,attempts),(True,True))
            self.assertEqual(read_error_evidence('gcli1',response,attempts),(False,True))
        self.assertEqual(read_error_evidence('cpa1',{'readError':None,'body':'data: [DONE]\n\n'},attempts),(False,True))
        self.assertEqual(read_error_evidence('cpa1',{'readError':None,'body':'data: {"error":{}}'},[]),(True,False))

    def test_direct_gcli_requires_actual_read_failure(self):
        self.assertEqual(read_error_evidence('gcli1',{'readError':'incomplete_read','body':''},[]),(True,False))


if __name__=='__main__':unittest.main()
