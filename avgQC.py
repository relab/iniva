import glob
import sys
import json
import csv
import numpy as np
import scipy.stats as st


def find_files(directory_name):
    if directory_name == None or directory_name == "":
        return -1
    files = glob.glob(directory_name+"/*/*.json")
    return files

def to_timestamp(timestamp_str):
    return isoparse(timestamp_str).timestamp()

def readFromFiles(directory_name):
    avg_qcs = []
    for file_name in find_files(directory_name):
        fp = open(file_name, )
        data = json.load(fp)
        for ele in data:
            if ele['@type'] == "type.googleapis.com/types.QCLength":
                avgQCLength = int(ele['AvgQCLength'])
                avg_qcs.append(avgQCLength)
    return avg_qcs

def compute_avg(qcs_length):
    average = sum(qcs_length)/len(qcs_length)
    interval = st.t.interval(confidence=0.90, df=len(qcs_length)-1, loc=np.mean(qcs_length), scale=st.sem(qcs_length))
    print(str(average))
    print(str(interval))

if __name__ == '__main__':
    if len(sys.argv) != 2:
        print("usage: python avgQC.py inputDirectory")
        exit()
    qcs_length = readFromFiles(sys.argv[1])
    #write_throughput_avg(sys.argv[1], latency_data, sys.argv[2])
    compute_avg(qcs_length)
