# Syncr Android spike: test kit for your phone

This kit runs the real `syncr-worker`, `rclone` and `restic`, built for Android, on your phone in Termux. It checks what the Android app will depend on, and writes everything to one text file for you to send back. It needs no root, no adb and no computer.

It takes about 45–90 minutes in total, most of it waiting. It needs about **3 GB of free space** on the phone: the kit (165 MB), 600 MB of test data on shared storage, and test data, repositories and copies in Termux's own folder. `cleanup` removes all of it at the end.

The report holds device facts, timings and test results. It never holds your enrollment token, the worker's secret, a password or a config file.

## 1. Install Termux

Install Termux from **F-Droid** (<https://f-droid.org/packages/com.termux/>) or from its **GitHub releases** (<https://github.com/termux/termux-app/releases>, the `…github-debug_arm64-v8a.apk`). **Not from Google Play**: that build may not run downloaded programs. Open it once and wait until the prompt (`~ $`) appears.

Termux is a Linux terminal. You type commands at the prompt, and long-pressing the screen gives Paste. The extra keys row has Ctrl, Esc and the arrows.

## 2. Get the kit

On the phone, sign in to GitHub in the browser and open the CI run you were sent. Under **Artifacts**, download `syncr-android-spike-arm64`. It is saved as a zip of about 60 MB (165 MB unpacked) in your Download folder.

## 3. Prepare Termux

```sh
pkg update                 # press Enter at each question
pkg install unzip
termux-setup-storage       # Android asks: allow access to files
```

`termux-setup-storage` lets Termux use shared storage (`/storage/emulated/0`) and creates `~/storage/downloads`.

## 4. Unpack

```sh
mkdir -p ~/syncr-kit && cd ~/syncr-kit
ls ~/storage/downloads/                       # find the zip's exact name
unzip ~/storage/downloads/syncr-android-spike-arm64.zip
chmod +x termux-spike.sh
```

The zip does not keep the execute permission. The script sets it on the binaries itself, so only it needs `chmod +x`.

## 5. Run the tests

Keep Termux open with the screen on while these run, or pull down Termux's notification and tap *Acquire wakelock*.

```sh
./termux-spike.sh all
```

This takes 20–40 minutes. It covers device facts, the binaries and their versions, DNS and TLS, the worker's own Go tests, and restic on synthetic data in `/storage/emulated/0/SyncrSpike`.

Then the screen-off test:

```sh
./termux-spike.sh long-start
```

Leave Termux with the home button, lock the screen, and leave the phone alone for **at least 15 minutes**, unplugged if you can. Then:

```sh
./termux-spike.sh long-check
```

If you want to, repeat the screen-off test without Termux's wake lock: `SPIKE_NO_WAKELOCK=1 ./termux-spike.sh long-start`, then `long-check` again.

## 6. Optional: the phone as a real worker

This needs your Syncr controller. First create an enrollment token: *Workers → Add worker*.

```sh
./termux-spike.sh worker
```

The script asks for the controller's address, a CA file (only for a self-signed controller) and the token. The token is not shown while you type. The worker is named after your phone, with `-spike` added. It may use only `/storage/emulated/0/SyncrSpike`. The script prints the steps to do in the Syncr UI: a backup, a restore, a transfer, and one run with the screen locked.

Afterwards:

```sh
./termux-spike.sh worker-check     # summary of the worker's log
./termux-spike.sh worker-stop
```

## 7. Send back the report

Send `~/syncr-spike/report.txt`. A copy is in the phone's Download folder as `syncr-spike-report.txt`, so you can share it from the Files app. For the worker test, also note the host name the snapshot shows in the UI.

## 8. Clean up

```sh
./termux-spike.sh cleanup
```

This removes the test data, the repositories and the worker files, and keeps the report. Remove the worker in the Syncr UI too, then uninstall Termux if you like.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `SPIKE_DATA_MB` | 300 | size of the synthetic data |
| `SPIKE_FILES` | 2000 | number of files in it |
| `SPIKE_LONG_MIN` | 10 | length of the screen-off job, in minutes |
| `SPIKE_NO_WAKELOCK` | | `1`: the screen-off job runs without a wake lock |
