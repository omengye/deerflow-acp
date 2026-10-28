"""Restore the exact pre-ASR2 files and pinned original running image."""
import argparse
import json
from pathlib import Path
from deploy import DeploymentError, rollback


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--deployment-report', type=Path, required=True)
    args = parser.parse_args()
    try:
        report = rollback(args.deployment_report)
        print(json.dumps(report, ensure_ascii=False), flush=True)
        return 0
    except DeploymentError as error:
        print(json.dumps({'status': 'failed', 'error': str(error)}), flush=True)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
