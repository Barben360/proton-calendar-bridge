# Human checks deferred to the author

Steps that need a person (CAPTCHA, GUI clients). Run them in order when
back; the agent resumes everything else from the results.

## 1. Solve the test account's CAPTCHA (unblocks every live experiment)

The first `pcb-lab login` on blackwater (2026-11-10 23:47 UTC) was answered
with a Proton CAPTCHA, as a new client usually is.

1. Show the verify URL (the token in it is a CAPTCHA challenge, not an
   account secret):
   `python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.local/state/pcb-lab/hv.json")))["url"])'`
2. Open it in any browser, solve the CAPTCHA, close the tab.
3. Tell the agent "captcha solved at HH:MM". It then runs, from a new
   process: `pass-cli run --env-file scripts/lab.env.example -- pcb-lab login --captcha-token <token from hv.json>`.
   - If Proton accepts the hours-old token: E02 learns that a solved token
     survives a long delay and another process.
   - If not, the agent starts one fresh login, you solve the new URL right
     away, and the agent resumes within a minute (E02 learns the limit).

After that the agent runs, without you: E03 measurements, E01 refresh
probes and the 14-day timer, the synthetic data set (Task 10).

## 2. Thunderbird against the bridge (after the CalDAV milestone)

Filled in when the CalDAV adapter exists.
