"""Restore pre-fix ASR2 source/image and its original v2 rollback metadata."""
import argparse
import json
from pathlib import Path
from deploy import DeploymentError, rollback


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--deployment-report', type=Path, required=True)
    args = parser.parse_args()
    try:
        print(json.dumps(rollback(args.deployment_report), ensure_ascii=False), flush=True)
        return 0
    except DeploymentError as error:
        print(json.dumps({'status': 'failed', 'error': str(error)}), flush=True)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
