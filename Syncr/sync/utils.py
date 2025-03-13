import json
import subprocess
from time import sleep
from . import models

def queryJob(jobId: int) -> dict:
    statusQuery = subprocess.run(["rclone", "rc",
                             "job/status",
                             "jobid=" + str(jobId),
                             "--rc-addr=127.0.0.1:5572"],
                            check=True,
                            stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE,)
    statusQuery = json.loads(statusQuery.stdout.decode())
    
    statsQuery = subprocess.run(["rclone", "rc",
                             "core/stats",
                             "group=job/" + str(jobId),
                             "--rc-addr=127.0.0.1:5572"],
                            check=True,
                            stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE,)
    statsQuery = json.loads(statsQuery.stdout.decode())
    # Remove the "transferring" and "eta" key if it exists
    statsQuery.pop("transferring", None)
    statsQuery.pop("eta", None)
    # Rename id to rcloneId
    statusQuery["rcloneId"] = statusQuery.pop("id")
    
    combinedQuery = {**statusQuery, **statsQuery}
    
    return combinedQuery

def createJobObject(jobId: int, request) -> int:
    combinedQuery = queryJob(jobId)
    # It only works if the keys are the same in the queries and the model
    jobObject = models.Job(
        user=request.user,
        **combinedQuery
    )
    
    jobObject.save()
    
    return jobObject.id

def queryJobStatus(jobId: int, modelId) -> None:
    jobObject = models.Job.objects.get(id=modelId)
    
    while(True):
        combinedQuery = queryJob(jobId)
        
        # Update the model with the new information
        # It may not be effient to do this way
        for key, value in combinedQuery.items():
            setattr(jobObject, key, value)
        
        # If the job is finished, break the loop
        if combinedQuery.get("finished"):
            print("Job finished")
            break
        
        print(f"Updated job {jobId}")
        sleep(1) # Only check every second